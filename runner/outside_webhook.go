// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/egress"
)

// An outside agent can have its wake-ups sent to its own address instead
// of checking an inbox: a POST of the same wake-up, signed the Standard
// Webhooks way (webhook-id, webhook-timestamp, webhook-signature: v1 and an
// HMAC-SHA256 of id.timestamp.body, with the secret after whsec_). It
// answers in the response ({"reply": ...} or {"fail": ...}), or takes the
// wake-up (any other 2xx) and answers later through the API. A failure is
// tried again with the same id; a refusal (3xx or 4xx) isn't. Only public
// HTTPS addresses are called (package egress).

// webhookClient is how webhooks are called.
func (r *Runner) webhookClient() *http.Client {
	r.hookOnce.Do(func() { r.hooks = egress.Client(r.cfg.WebhookEgress) })
	return r.hooks
}

// sealKey is the key webhook secrets are sealed with in the org database:
// from the network's own secret, which only its runner has (never the API
// token, which the console has too).
func (r *Runner) sealKey() ([]byte, error) {
	if r.cfg.Accounts.Secret == "" {
		return nil, errNoSealKey
	}
	sum := sha256.Sum256([]byte("guildnet webhook secrets\x00" + r.cfg.Accounts.Secret))
	return sum[:], nil
}

var errNoSealKey = errors.New("webhooks need the network's own secret (GUILDNET_ACCOUNT_SECRET)")

// webhookSealed starts a webhook secret sealed bound to its agent and
// address; one sealed before has nothing in front, and is sealed again so
// the first time it's read.
const webhookSealed = 0x01

// webhookAAD is what a webhook's secret is sealed to: its agent and its
// address, so it isn't sent anywhere else, or for another agent.
func webhookAAD(nick, url string) []byte {
	return []byte("agent_webhooks|secret|" + strings.ToLower(nick) + "|" + url)
}

func (r *Runner) gcm() (cipher.AEAD, error) {
	key, err := r.sealKey()
	if err != nil {
		return nil, err
	}
	return gcmWith(key)
}

func gcmWith(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (r *Runner) seal(plain, aad []byte) ([]byte, error) {
	gcm, err := r.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(append([]byte{webhookSealed}, nonce...), nonce, plain, aad), nil
}

// unseal opens a webhook secret sealed to aad; legacy says it was sealed
// before secrets were bound to their agent and address.
func (r *Runner) unseal(sealed, aad []byte) (plain []byte, legacy bool, err error) {
	gcm, err := r.gcm()
	if err != nil {
		return nil, false, err
	}
	n := gcm.NonceSize()
	if len(sealed) > n && sealed[0] == webhookSealed {
		if plain, err := gcm.Open(nil, sealed[1:1+n], sealed[1+n:], aad); err == nil {
			return plain, false, nil
		}
	}
	if len(sealed) >= n {
		if plain, err := gcm.Open(nil, sealed[:n], sealed[n:], nil); err == nil {
			return plain, true, nil
		}
	}
	return nil, false, errors.New("couldn't unseal it: sealed with another key, or moved from the agent and address it was set for")
}

// webhookOf is where p's wake-ups are sent and the key they're signed with;
// "" for the inbox.
func (r *Runner) webhookOf(ctx context.Context, p config.Persona) (string, []byte, error) {
	url, sealed, err := r.store.AgentWebhook(ctx, p.Nick)
	if err != nil || url == "" {
		return "", nil, err
	}
	key, legacy, err := r.unseal(sealed, webhookAAD(p.Nick, url))
	if err != nil {
		return "", nil, fmt.Errorf("reading %s's webhook secret: %w", p.Nick, err)
	}
	if legacy {
		// Sealed before secrets were bound: sealed again, bound, now.
		if again, err := r.seal(key, webhookAAD(p.Nick, url)); err == nil {
			r.store.SetAgentWebhook(ctx, p.Nick, url, again)
		}
	}
	return url, key, nil
}

// setAgentWebhook sends an outside agent's wake-ups to url, with a new
// secret, returned once; url "" goes back to the inbox.
func (r *Runner) setAgentWebhook(ctx context.Context, actor, nick, url string) (string, error) {
	p, err := r.store.Agent(ctx, nick)
	if err != nil {
		return "", r.storeRefusal(nick, err)
	}
	if !r.can(actor, canHire, onPerson(p.Nick)) {
		return "", refuse(forbidden, "you can't change %s, so you can't set where its work goes.", p.Nick)
	}
	if p.Platform != PlatformOutside {
		return "", refuse(invalid, "%s doesn't run outside GuildNet, so there's nothing to send it.", p.Nick)
	}
	url = strings.TrimSpace(url)
	if url == "" {
		if err := r.store.SetAgentWebhook(ctx, p.Nick, "", nil); err != nil {
			return "", r.storeRefusal(p.Nick, err)
		}
		r.logChange(actor, "webhook", p.Nick, p.Team, "none: its program checks its inbox")
		return "", nil
	}
	if len(url) > 500 {
		return "", refuse(invalid, "that address is too long.")
	}
	if err := egress.CheckURL(url); err != nil {
		return "", refuse(invalid, "GuildNet can't send work to %s: %v.", url, err)
	}
	if _, err := r.sealKey(); err != nil {
		return "", refuse(invalid, "%v; until it's set, %s's program checks its inbox.", err, p.Nick)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	sealed, err := r.seal(key, webhookAAD(p.Nick, url))
	if err != nil {
		return "", err
	}
	if err := r.store.SetAgentWebhook(ctx, p.Nick, url, sealed); err != nil {
		return "", r.storeRefusal(p.Nick, err)
	}
	r.logChange(actor, "webhook", p.Nick, p.Team, url)
	return "whsec_" + base64.StdEncoding.EncodeToString(key), nil
}

func (r *Runner) apiAgentWebhook(w http.ResponseWriter, req *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decode(w, req, &body) {
		return
	}
	secret, err := r.setAgentWebhook(req.Context(), actorOf(req), req.PathValue("nick"), body.URL)
	if err != nil {
		apiRefusal(w, err)
		return
	}
	out := map[string]string{}
	if secret != "" {
		out["secret"] = secret
	}
	apiJSON(w, http.StatusOK, out)
}

// deliverWake gets a wake-up to an outside agent: to its webhook if it has
// one, otherwise its inbox; and waits for the answer.
func (r *Runner) deliverWake(ctx context.Context, p config.Persona, w wakeUp, onStep func(agent.Step)) (answer, error) {
	url, key, err := r.webhookOf(ctx, p)
	if err != nil {
		return answer{}, err
	}
	if url == "" {
		if !r.outside.connected(p.Nick, r.now()) {
			return answer{}, errNotConnected
		}
		return r.outside.deliver(ctx, p.Nick, w, onStep)
	}
	x := r.outside.register(p.Nick, w, onStep)
	defer r.outside.forget(w.ID)
	a, now, err := r.sendWebhook(ctx, url, key, w)
	r.outside.delivered(p.Nick, err)
	if err != nil {
		return answer{}, err
	}
	if now {
		return a, nil
	}
	select {
	case a := <-x.done:
		return a, nil
	case <-ctx.Done():
		return answer{}, fmt.Errorf("its webhook took the wake-up but didn't answer in time: %w", ctx.Err())
	}
}

// sendWebhook posts w to url, signed with key, trying a failure again; now
// says the answer came in the response.
func (r *Runner) sendWebhook(ctx context.Context, url string, key []byte, w wakeUp) (a answer, now bool, err error) {
	body, err := json.Marshal(w)
	if err != nil {
		return answer{}, false, err
	}
	wait := r.cfg.WebhookRetryAfter
	if wait <= 0 {
		wait = 2 * time.Second
	}
	var last error
	for attempt, pause := range []time.Duration{0, wait, 4 * wait, 15 * wait} {
		if attempt > 0 {
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return answer{}, false, fmt.Errorf("its webhook failed (%v), and there was no time to try again", last)
			}
		}
		a, now, retry, err := r.postWebhook(ctx, url, key, w.ID, body)
		if err == nil {
			return a, now, nil
		}
		last = err
		if !retry {
			return answer{}, false, err
		}
	}
	return answer{}, false, fmt.Errorf("its webhook failed four times: %w", last)
}

// postWebhook makes one call; retry says a failure is worth trying again.
func (r *Runner) postWebhook(ctx context.Context, url string, key []byte, id string, body []byte) (a answer, now, retry bool, err error) {
	ts := strconv.FormatInt(r.now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + string(body)))
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return answer{}, false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GuildNet-Webhooks/1")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	resp, err := r.webhookClient().Do(req)
	if err != nil {
		return answer{}, false, true, fmt.Errorf("calling its webhook: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAgentText*4))
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return answer{}, false, true, fmt.Errorf("its webhook answered %s", resp.Status)
	case resp.StatusCode >= 300:
		return answer{}, false, false, fmt.Errorf("its webhook refused the wake-up: %s", resp.Status)
	}
	var out struct {
		Reply *string `json:"reply"`
		Fail  string  `json:"fail"`
	}
	json.Unmarshal(raw, &out)
	switch {
	case out.Fail != "":
		return answer{gaveUp: strings.Join(strings.Fields(cleanText(out.Fail)), " ")}, true, false, nil
	case out.Reply != nil:
		text := cleanText(*out.Reply)
		if len(text) > maxAgentText {
			text = text[:maxAgentText]
		}
		if text == "" {
			text = agent.Pass
		}
		return answer{reply: text}, true, false, nil
	}
	return answer{}, false, false, nil // taken; the answer comes through the API
}
