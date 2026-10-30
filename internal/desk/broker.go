// SPDX-License-Identifier: AGPL-3.0-only

package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Broker is the platform's broker, as a network's runner asks it about
// desks: where the sandbox is, with the network's token, and to wake the
// sandbox VM when it's asleep.
type Broker struct {
	URL, Network, Secret string
	// HTTP is how the broker is reached; nil means a plain client.
	HTTP *http.Client
	// pin is the key the broker last said sandboxd has.
	pin atomic.Pointer[string]
}

// Pin is the key the broker last said sandboxd has (pki.KeyPin): "" if it
// said none, or hasn't been asked.
func (b *Broker) Pin() string {
	if p := b.pin.Load(); p != nil {
		return *p
	}
	return ""
}

var brokerClient = &http.Client{Timeout: 15 * time.Second}

func (b *Broker) do(ctx context.Context, method, path string) (*http.Response, error) {
	return b.send(ctx, method, path, nil)
}

func (b *Broker) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		in = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.URL, "/")+path, in)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("GuildNet-Network", b.Network)
	req.Header.Set("Authorization", "Bearer "+b.Secret)
	client := b.HTTP
	if client == nil {
		client = brokerClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking the broker: %w", err)
	}
	return resp, nil
}

// Sandbox is where the network's desks are, and a token for them; nothing,
// with no error, if the platform has no sandbox.
func (b *Broker) Sandbox(ctx context.Context) (url, token string, err error) {
	s, err := b.sandbox(ctx)
	return s.URL, s.Token, err
}

// Pass is the network's egress pass, which opens its git repositories'
// hosts at the desks' proxy, and hosts (the owner's list, on 443) as the
// platform allows: asked for fresh, as the owner may have added one. "" if
// it opens none.
func (b *Broker) Pass(ctx context.Context, hosts []string) (string, error) {
	s, err := b.sandboxFor(ctx, hosts)
	return s.Egress, err
}

// KeptPass is a network's egress pass from get, for the hosts hosts gives,
// kept for keep (well inside the 15 minutes a pass is good for, and the 10
// a command may run): asked for again sooner only when the hosts change.
// now is the clock; nil means time.Now.
func KeptPass(get func(ctx context.Context, hosts []string) (string, error), hosts func() []string, keep time.Duration, now func() time.Time) func(context.Context) (string, error) {
	if now == nil {
		now = time.Now
	}
	var mu sync.Mutex
	var pass, kept string
	var at time.Time
	return func(ctx context.Context) (string, error) {
		h := hosts()
		key := strings.Join(h, ",")
		mu.Lock()
		defer mu.Unlock()
		if pass != "" && key == kept && now().Sub(at) < keep {
			return pass, nil
		}
		p, err := get(ctx, h)
		if err != nil {
			return "", err
		}
		pass, kept, at = p, key, now()
		return pass, nil
	}
}

// sandboxAnswer is the broker's answer about the sandbox.
type sandboxAnswer struct {
	URL     string    `json:"url"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
	Egress  string    `json:"egress"`
	Pin     string    `json:"pin"`
}

func (b *Broker) sandbox(ctx context.Context) (sandboxAnswer, error) { return b.sandboxFor(ctx, nil) }

// sandboxFor asks with the hosts the network's pass should open too.
func (b *Broker) sandboxFor(ctx context.Context, hosts []string) (sandboxAnswer, error) {
	var out sandboxAnswer
	path := "/sandbox"
	if len(hosts) > 0 {
		q := url.Values{"host": hosts}
		path += "?" + q.Encode()
	}
	resp, err := b.do(ctx, "GET", path)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return out, nil
	default:
		return out, fmt.Errorf("the broker didn't say where the sandbox is: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.URL == "" || out.Token == "" {
		return sandboxAnswer{}, fmt.Errorf("the broker's answer wasn't a sandbox")
	}
	b.pin.Store(&out.Pin)
	return out, nil
}

// renewBefore is how long before a desk token runs out another is asked for.
const renewBefore = 2 * time.Minute

// Tokens are the network's desk tokens from the broker: one is kept until
// shortly before it runs out (or a fresh one is asked for), then another
// is asked for.
func (b *Broker) Tokens() TokenSource {
	var mu sync.Mutex
	var token string
	var until time.Time
	return func(ctx context.Context, fresh bool) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !fresh && token != "" && time.Now().Before(until.Add(-renewBefore)) {
			return token, nil
		}
		s, err := b.sandbox(ctx)
		if err != nil {
			return "", err
		}
		t, expires := s.Token, s.Expires
		if t == "" {
			return "", errors.New("the platform has no sandbox now")
		}
		if expires.IsZero() {
			expires = time.Now().Add(renewBefore * 2) // an older platform's: ask again soon
		}
		token, until = t, expires
		return token, nil
	}
}

// Turn is the platform's answer to a desk asking to work.
type Turn struct {
	// ID is what the desk's time is reported against; "" means it isn't
	// counted here.
	ID string
	// Busy says the plan's desks are all working: ask again soon.
	Busy bool
	// Refused says why the desk may not work (its network is out of desk
	// hours and credits), if it may not.
	Refused string
}

// StartDesk asks whether agent's desk may work now: resume says it's
// carrying on after an approval, which isn't refused for hours.
func (b *Broker) StartDesk(ctx context.Context, agent string, resume bool) (Turn, error) {
	resp, err := b.send(ctx, "POST", "/desks/start", map[string]any{"agent": agent, "resume": resume})
	if err != nil {
		return Turn{}, err
	}
	defer resp.Body.Close()
	var out struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	switch resp.StatusCode {
	case http.StatusOK:
		if out.ID == "" {
			return Turn{}, fmt.Errorf("the broker's answer had no desk")
		}
		return Turn{ID: out.ID}, nil
	case http.StatusNotFound:
		return Turn{}, nil
	case http.StatusTooManyRequests:
		return Turn{Busy: true}, nil
	case http.StatusPaymentRequired:
		if out.Reason == "" {
			out.Reason = "the network is out of desk hours"
		}
		return Turn{Refused: out.Reason}, nil
	}
	return Turn{}, fmt.Errorf("the broker didn't say whether the desk may work: %s", resp.Status)
}

// DeskDone says how long a desk worked.
func (b *Broker) DeskDone(ctx context.Context, id string, seconds int64) error {
	resp, err := b.send(ctx, "POST", "/desks/done", map[string]any{"id": id, "seconds": seconds})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the broker didn't take the desk's time: %s", resp.Status)
	}
	return nil
}

// BeatDesk says a desk is still working, so its turn isn't taken for lost.
func (b *Broker) BeatDesk(ctx context.Context, id string) error {
	resp, err := b.send(ctx, "POST", "/desks/beat", map[string]any{"id": id})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the broker didn't take the desk's word that it's still working: %s", resp.Status)
	}
	return nil
}

// Wake asks for the sandbox VM to be started.
func (b *Broker) Wake(ctx context.Context) error {
	resp, err := b.do(ctx, "POST", "/sandbox/wake")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("the broker didn't wake the sandbox: %s", resp.Status)
	}
	return nil
}
