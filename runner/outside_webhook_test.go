// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/egress"
)

// An outside agent can have its wake-ups sent to its own address (a
// webhook) instead of checking an inbox: a signed POST (Standard Webhooks)
// of the same wake-up, answered in the response or later through the API.
// GuildNet only calls public HTTPS addresses (the egress guard).

// hook is an agent's web service, on this machine but reached by a public
// name: it checks each call's signature and answers with answer.
type hook struct {
	t      *testing.T
	srv    *httptest.Server
	secret string
	answer func(attempt int, wake map[string]any) (int, string)

	mu       sync.Mutex
	calls    []map[string]any
	ids      []string
	badSigns int
}

const hookHost = "kai-agent.example.com"

func newHook(t *testing.T, answer func(attempt int, wake map[string]any) (int, string)) *hook {
	h := &hook{t: t, answer: answer}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id, ts := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp")
		h.mu.Lock()
		if !h.signed(id, ts, body, r.Header.Get("webhook-signature")) {
			h.badSigns++
		}
		var wake map[string]any
		json.Unmarshal(body, &wake)
		h.calls = append(h.calls, wake)
		h.ids = append(h.ids, id)
		attempt := len(h.calls)
		h.mu.Unlock()
		status, reply := h.answer(attempt, wake)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// signed checks a Standard Webhooks signature: HMAC-SHA256 of
// id.timestamp.body, with the secret after whsec_, base64.
func (h *hook) signed(id, ts string, body []byte, sig string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h.secret, "whsec_"))
	if err != nil || id == "" {
		return false
	}
	at, _ := strconv.ParseInt(ts, 10, 64)
	if time.Since(time.Unix(at, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + string(body)))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, s := range strings.Fields(sig) {
		if hmac.Equal([]byte(s), []byte(want)) {
			return true
		}
	}
	return false
}

func (h *hook) url() string {
	_, port, _ := strings.Cut(strings.TrimPrefix(h.srv.URL, "https://"), ":")
	return "https://" + hookHost + ":" + port + "/guildnet"
}

// reachingHooks lets the runner reach test hooks by their public name.
func reachingHooks(h *hook) option {
	return func(cfg *config.Runner, _ *[]config.Persona) {
		pool := x509.NewCertPool()
		pool.AddCert(h.srv.Certificate())
		cfg.WebhookEgress = egress.Options{RootCAs: pool, AllowLoopback: true,
			Resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}}
		cfg.WebhookRetryAfter = 50 * time.Millisecond
	}
}

// withWebhook makes kai an outside agent whose wake-ups go to h.
func withWebhook(t *testing.T, n *network, h *hook) string {
	t.Helper()
	token := outside(t, n)
	status, out := n.api("method").call("POST", "/v1/staff/kai/webhook", map[string]any{"url": h.url()})
	secret, _ := out["secret"].(string)
	if status != http.StatusOK || !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("setting kai's webhook: %d %v", status, out)
	}
	h.secret = secret
	return token
}

func TestAWebhookAgentIsSentItsWakeUpsSignedAndAnswersInTheResponse(t *testing.T) {
	h := newHook(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, `{"reply": "Answered from my webhook."}`
	})
	n := startNetwork(t, &fakeBrain{}, withAPI(), reachingHooks(h))
	withWebhook(t, n, h)

	n.human.say("#engineering", "kai: is the release ready?")

	n.human.expect("kai's answer, from its webhook", fromContaining("kai", "#engineering", "Answered from my webhook."))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 1 || h.badSigns != 0 {
		t.Fatalf("%d calls, %d badly signed", len(h.calls), h.badSigns)
	}
	if w := h.calls[0]; w["type"] != "wake" || w["where"] != "#engineering" || w["id"] != h.ids[0] {
		t.Errorf("the wake-up sent: %v (webhook-id %s)", w, h.ids[0])
	}
	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); out["gets_work"] != "webhook" || out["webhook_url"] != h.url() {
		t.Errorf("kai's page: gets work %v at %v", out["gets_work"], out["webhook_url"])
	}
}

// A webhook's secret is sealed to its agent and address: moved in the
// database to send kai's wake-ups (signed with it) somewhere else, it
// doesn't open, and nothing is sent there.
func TestAWebhookSecretGoesOnlyWhereItWasSetFor(t *testing.T) {
	h := newHook(t, func(int, map[string]any) (int, string) { return http.StatusOK, `{"reply": "from the hook"}` })
	elsewhere := newHook(t, func(int, map[string]any) (int, string) { return http.StatusOK, `{"reply": "from elsewhere"}` })
	n := startNetwork(t, &fakeBrain{}, withAPI(), reachingHooks(h))
	withWebhook(t, n, h)
	ctx := context.Background()
	_, sealed, err := n.store.AgentWebhook(ctx, "kai")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.SetAgentWebhook(ctx, "kai", elsewhere.url(), sealed); err != nil {
		t.Fatal(err)
	}

	n.human.say("#engineering", "kai: is the release ready?")

	n.human.expect("kai can't be reached", fromContaining("ledger", "#ops", "kai couldn't finish"))
	elsewhere.mu.Lock()
	defer elsewhere.mu.Unlock()
	if len(elsewhere.calls) != 0 {
		t.Errorf("kai's wake-up went to the address put in the database: %v", elsewhere.calls)
	}
}

// A webhook's secret is sealed with the network's own secret, which only its
// runner has: without one, there are no webhooks (never a key the console
// knows too, such as the API token).
func TestWebhooksNeedTheNetworksOwnSecret(t *testing.T) {
	h := newHook(t, func(int, map[string]any) (int, string) { return http.StatusOK, `{"reply": "hi"}` })
	n := startNetwork(t, &fakeBrain{}, withAPI(), reachingHooks(h), func(cfg *config.Runner, _ *[]config.Persona) { cfg.Accounts.Secret = "" })
	outside(t, n)

	status, out := n.api("method").call("POST", "/v1/staff/kai/webhook", map[string]any{"url": h.url()})

	if status == http.StatusOK || !strings.Contains(errorOf(out), "GUILDNET_ACCOUNT_SECRET") {
		t.Errorf("setting a webhook without the network's secret: %d %v", status, out)
	}
}

// A webhook can take the wake-up (202) and answer later, through the API.
func TestAWebhookAgentCanAnswerLater(t *testing.T) {
	var n *network
	var token string
	h := newHook(t, func(_ int, wake map[string]any) (int, string) {
		go n.outsider(token).call("POST", "/wakes/"+wake["id"].(string)+"/reply", map[string]any{"text": "Answered later."})
		return http.StatusAccepted, ""
	})
	n = startNetwork(t, &fakeBrain{}, withAPI(), reachingHooks(h))
	token = withWebhook(t, n, h)

	n.human.say("#engineering", "kai: have a think")

	n.human.expect("kai's later answer", fromContaining("kai", "#engineering", "Answered later."))
}

// A webhook that's down is tried again (the same id, so the agent can tell);
// one that refuses isn't.
func TestAFailingWebhookIsRetriedAndARefusalIsReported(t *testing.T) {
	h := newHook(t, func(attempt int, _ map[string]any) (int, string) {
		if attempt < 3 {
			return http.StatusServiceUnavailable, ""
		}
		return http.StatusOK, `{"reply": "Third time lucky."}`
	})
	n := startNetwork(t, &fakeBrain{}, withAPI(), reachingHooks(h))
	withWebhook(t, n, h)

	n.human.say("#engineering", "kai: ping")

	n.human.expect("kai's answer after retries", fromContaining("kai", "#engineering", "Third time lucky."))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ids) != 3 || h.ids[0] != h.ids[2] {
		t.Errorf("ids %v: want three tries of the same wake-up", h.ids)
	}
}

func TestAWebhookThatRefusesIsntRetriedAndSaysSo(t *testing.T) {
	h := newHook(t, func(int, map[string]any) (int, string) { return http.StatusForbidden, `{"error": "nope"}` })
	n := startNetwork(t, &fakeBrain{}, withAPI(), reachingHooks(h))
	withWebhook(t, n, h)

	n.human.say("#engineering", "kai: ping")

	n.human.expect("kai couldn't answer", fromContaining("kai", "#engineering", "couldn't answer"))
	n.human.expect("why, in #ops", fromContaining("ledger", "#ops", "403"))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 1 {
		t.Errorf("a refusal was tried %d times", len(h.calls))
	}
}

// Only public HTTPS addresses, set by someone who may change the agent; the
// secret is shown once.
func TestAWebhookMustBeAPublicHTTPSAddress(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	outside(t, n)
	for _, bad := range []string{"http://agents.example.com/hook", "https://10.0.0.5/hook", "https://169.254.169.254/", "https://localhost/hook"} {
		if status, _ := n.api("method").call("POST", "/v1/staff/kai/webhook", map[string]any{"url": bad}); status != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, status)
		}
	}
	if status, out := n.api("method").call("POST", "/v1/staff/kai/webhook", map[string]any{"url": "https://agents.example.com/hook"}); status != http.StatusOK || out["secret"] == nil {
		t.Errorf("a public address: %d %v", status, out)
	}
	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); strings.Contains(jsonText(out), "whsec_") {
		t.Error("the secret is shown again")
	}
	if status, _ := n.api("method").call("POST", "/v1/staff/kai/webhook", map[string]any{"url": ""}); status != http.StatusOK {
		t.Errorf("going back to the inbox: %d", status)
	}
	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); out["gets_work"] != "inbox" {
		t.Errorf("after clearing: gets work %v", out["gets_work"])
	}
}

// A webhook's address often carries a secret of its own (in its path): it's
// shown only to those who may change the agent, the rest see where it goes
// (the host), and the org's log keeps only the host.
func TestAWebhooksAddressIsShownOnlyToThoseWhoMayChangeTheAgent(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	outside(t, n)
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Designer" TEAM Marketing`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	const url = "https://hooks.example.com/catch/8812/s3cr3t-path"
	if status, out := n.api("method").call("POST", "/v1/staff/kai/webhook", map[string]any{"url": url}); status != http.StatusOK {
		t.Fatalf("setting the webhook: %d %v", status, out)
	}

	_, owner := n.api("method").call("GET", "/v1/staff/kai", nil)
	_, member := n.api("dana").call("GET", "/v1/staff/kai", nil)
	_, log := n.api("method").call("GET", "/v1/log", nil)

	if owner["webhook_url"] != url {
		t.Errorf("the owner sees %v, want the whole address", owner["webhook_url"])
	}
	if member["gets_work"] != "webhook" || member["webhook_host"] != "hooks.example.com" || strings.Contains(jsonText(member), "s3cr3t") {
		t.Errorf("dana, who can't change kai, sees %v", member)
	}
	if text := jsonText(log); strings.Contains(text, "s3cr3t") || !strings.Contains(text, "hooks.example.com") {
		t.Errorf("the org's log: %s", text)
	}
}
