// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/limit"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Agents that run outside GuildNet reach their network here, at
// https://<network>.guildnet.chat/agents/v1: their program calls with its
// agent's token, and the platform passes the call on to the network's
// runner with the network's secret. The runner checks the token and knows
// which agent it is. No cookies or sessions are involved.

const (
	// agentCallsAMinute is how many calls a token may make a minute, besides
	// checking its inbox.
	agentCallsAMinute = 60
	// agentPolls is how many inbox checks a token may have open at once.
	agentPolls = 2
)

// agentClient reaches runners for agents: long enough for an inbox check
// held open (up to 25 seconds). A redirect is answered as it is, not
// followed: following it would send the network's secret on to wherever
// it points.
var agentClient = &http.Client{Timeout: 40 * time.Second, CheckRedirect: noRedirects}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// agentNetworkCalls is how many calls a network's agents may make a minute
// between them. A token the runner hasn't taken lately isn't counted there,
// but against agentUnknownCalls a minute from where it comes from (an
// address, or an IPv6 /64), so made-up tokens can't crowd the agents out;
// one the runner refused is refused here for agentRefusedFor.
const (
	agentNetworkCalls = 600
	agentUnknownCalls = 30
	agentKnownFor     = time.Hour
	agentRefusedFor   = 10 * time.Minute
)

// plainPath says the path climbs nowhere: no "..", ".", or doubled slashes
// that would take a call out of the API it was sent to.
func plainPath(p string) bool { return path.Clean(p) == p }

// agentLimits count each token's calls this minute, and its open inbox
// checks. Tokens are counted by hash.
type agentLimits struct {
	calls limit.Minute
	mu    sync.Mutex
	polls map[string]int
	// The runner's last word on each token: taken, or refused, and when.
	taken, refused map[string]time.Time
}

// heard keeps what the runner said of a token: taken (any answer but 401)
// or refused.
func (l *agentLimits) heard(token string, ok bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.taken == nil {
		l.taken, l.refused = map[string]time.Time{}, map[string]time.Time{}
	}
	if len(l.taken)+len(l.refused) > 10000 {
		for k, at := range l.taken {
			if now.Sub(at) > agentKnownFor {
				delete(l.taken, k)
			}
		}
		for k, at := range l.refused {
			if now.Sub(at) > agentRefusedFor {
				delete(l.refused, k)
			}
		}
	}
	if ok {
		l.taken[token] = now
		delete(l.refused, token)
	} else {
		l.refused[token] = now
		delete(l.taken, token)
	}
}

// standing is what the runner said of a token lately: known (taken), or
// refused.
func (l *agentLimits) standing(token string, now time.Time) (known, refused bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.taken[token]; ok && now.Sub(at) < agentKnownFor {
		return true, false
	}
	at, ok := l.refused[token]
	return false, ok && now.Sub(at) < agentRefusedFor
}

// call counts a call, or says to wait (and for how long) if it's one more
// than max this minute.
func (l *agentLimits) call(token string, now time.Time, max int) (ok bool, retry time.Duration) {
	return l.calls.Call(token, now, max)
}

// poll opens an inbox check, if the token hasn't too many open; done closes it.
func (l *agentLimits) poll(token string) (ok bool, done func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.polls == nil {
		l.polls = map[string]int{}
	}
	if l.polls[token] >= agentPolls {
		return false, nil
	}
	l.polls[token]++
	return true, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.polls[token]--; l.polls[token] <= 0 {
			delete(l.polls, token)
		}
	}
}

func agentError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// agentAPI passes an outside agent's call on to its network's runner.
func (p *Platform) agentAPI(w http.ResponseWriter, r *http.Request, n Network) {
	if !strings.HasPrefix(r.URL.Path, "/agents/v1/") || !plainPath(r.URL.Path) {
		agentError(w, http.StatusNotFound, "there's nothing here; the agent API is at /agents/v1")
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "gna_") || len(token) > 200 {
		agentError(w, http.StatusUnauthorized, "send the agent's token: Authorization: Bearer gna_...")
		return
	}
	if n.Status == Suspended {
		agentError(w, http.StatusServiceUnavailable, "this network is suspended")
		return
	}
	sum := sha256.Sum256([]byte(n.Name + "\x00" + token))
	counted := hex.EncodeToString(sum[:])
	now := p.opts.Now()
	known, refused := p.agentLimits.standing(counted, now)
	if refused {
		agentError(w, http.StatusUnauthorized, "that token isn't an agent's here")
		return
	}
	if !known {
		if ok, retry := p.agentLimits.call("unknown\x00"+n.Name+"\x00"+signInSource(p.visitor(r)), now, agentUnknownCalls); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())))
			agentError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d calls a minute from here with tokens not yet seen", agentUnknownCalls))
			return
		}
	} else if ok, retry := p.agentLimits.call("network\x00"+n.Name, now, agentNetworkCalls); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())))
		agentError(w, http.StatusTooManyRequests, fmt.Sprintf("this network's agents may make at most %d calls a minute between them", agentNetworkCalls))
		return
	}
	if r.URL.Path == "/agents/v1/inbox" {
		ok, done := p.agentLimits.poll(counted)
		if !ok {
			w.Header().Set("Retry-After", "5")
			agentError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d inbox checks open at once", agentPolls))
			return
		}
		defer done()
	} else if ok, retry := p.agentLimits.call(counted, p.opts.Now(), agentCallsAMinute); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())))
		agentError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d calls a minute, besides checking the inbox", agentCallsAMinute))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentClient.Timeout)
	defer cancel()
	target := strings.TrimRight(n.RunnerURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		agentError(w, http.StatusBadRequest, "that isn't a request the agent API takes")
		return
	}
	req.Header.Set("Authorization", "Bearer "+n.RunnerToken)
	req.Header.Set("GuildNet-Agent-Token", token)
	// What the call says about itself, and MCP's own headers; nothing else
	// (no cookies) goes on.
	for _, h := range []string{"Content-Type", "Accept", "Mcp-Protocol-Version", "Mcp-Session-Id", "Last-Event-ID"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := agentClient.Do(req)
	if err != nil {
		monitor.Problem(p.opts.Logger, "agent API for %s: %v", n.Name, err)
		agentError(w, http.StatusBadGateway, "the network didn't answer; try again in a moment")
		return
	}
	defer resp.Body.Close()
	p.agentLimits.heard(counted, resp.StatusCode != http.StatusUnauthorized, p.opts.Now())
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	for _, h := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}
