// SPDX-License-Identifier: AGPL-3.0-only

package brain_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/brain"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/pki"
)

// fakeProvider is a model provider's API: it records what it was asked and
// answers with body.
type fakeProvider struct {
	mu       sync.Mutex
	requests []map[string]any
	paths    []string
	auth     []string
	body     string
	queued   []string // answers for the next calls, in order, before body
}

func newFakeProvider(t *testing.T, body string) (*fakeProvider, string) {
	f := &fakeProvider{body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		f.mu.Lock()
		f.requests, f.paths = append(f.requests, req), append(f.paths, r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization")+r.Header.Get("X-Api-Key"))
		body := f.body
		if len(f.queued) > 0 {
			body, f.queued = f.queued[0], f.queued[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1/"
}

func (f *fakeProvider) last(t *testing.T) (map[string]any, string, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the provider was never asked")
	}
	n := len(f.requests) - 1
	return f.requests[n], f.paths[n], f.auth[n]
}

// queue has the next calls answered with bodies, in order.
func (f *fakeProvider) queue(bodies ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, bodies...)
}

// request is the nth call's request (from 0).
func (f *fakeProvider) request(t *testing.T, n int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		t.Fatalf("the provider was asked %d times, not %d", len(f.requests), n+1)
	}
	return f.requests[n]
}

func (f *fakeProvider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeProvider) setBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

// completion is an OpenAI-style answer with usage.
func completion(text, usage string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":` +
		quote(text) + `,"refusal":null}}],"usage":` + usage + `}`
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func wake(model, effort string) agent.Request {
	return agent.Request{
		Persona:  config.Persona{Nick: "kai", Name: "Kai", Role: "Backend engineer", Model: model, Effort: effort, ReportsTo: "method"},
		Where:    "#engineering",
		Location: time.UTC,
		Trigger:  chat.Line{Nick: "method", Text: "kai: is the build green?"},
	}
}

func brainWith(p models.Provider, url string) *brain.Brain {
	return brain.New(brain.Options{Company: "Acme", Owner: "method",
		Keys: map[models.Provider]string{p: "key-" + string(p)}, BaseURLs: map[models.Provider]string{p: url}})
}

// An agent on an OpenAI model is answered by OpenAI, with its key, the
// model's own name, and its effort; cached input and reasoning (which
// OpenAI counts in the output) are charged as such.
func TestAnAgentOnAnOpenAIModelIsAnsweredByOpenAI(t *testing.T) {
	fake, url := newFakeProvider(t, completion("It's green.", `{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200,
		"prompt_tokens_details":{"cached_tokens":400},"completion_tokens_details":{"reasoning_tokens":50}}`))
	b := brainWith(models.OpenAI, url)

	res, err := b.Respond(context.Background(), wake("openai:gpt-6-sol", "high"), nil)

	if err != nil || res.Reply != "It's green." {
		t.Fatalf("reply %q, %v", res.Reply, err)
	}
	req, path, auth := fake.last(t)
	if path != "/v1/chat/completions" || auth != "Bearer key-openai" || req["model"] != "gpt-6-sol" {
		t.Errorf("asked %s with %q for %v", path, auth, req["model"])
	}
	if req["reasoning_effort"] != "high" || req["max_completion_tokens"] == nil {
		t.Errorf("effort %v, max_completion_tokens %v", req["reasoning_effort"], req["max_completion_tokens"])
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 || !strings.Contains(msgs[0].(map[string]any)["content"].(string), "You are Kai") ||
		!strings.Contains(msgs[1].(map[string]any)["content"].(string), "is the build green?") {
		t.Errorf("messages = %v", msgs)
	}
	if res.Usage != (ledger.Usage{Input: 600, CacheRead: 400, Output: 200}) || res.Cost != 0 {
		t.Errorf("usage %+v cost %v", res.Usage, res.Cost)
	}
}

// Gemini (through its OpenAI-compatible API) takes max_tokens, and counts
// thinking apart from the output; thinking still bills as output.
func TestGeminiThinkingIsChargedAsOutput(t *testing.T) {
	fake, url := newFakeProvider(t, completion("Green.", `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":150,
		"completion_tokens_details":{"reasoning_tokens":30}}`))
	res, err := brainWith(models.Google, url).Respond(context.Background(), wake("google:gemini-3.8-flash", "low"), nil)

	req, _, auth := fake.last(t)
	if err != nil || auth != "Bearer key-google" || req["model"] != "gemini-3.8-flash" || req["max_tokens"] == nil {
		t.Errorf("asked with %q for %v, max_tokens %v: %v", auth, req["model"], req["max_tokens"], err)
	}
	if res.Usage != (ledger.Usage{Input: 100, Output: 50}) {
		t.Errorf("usage %+v, want the thinking counted as output", res.Usage)
	}
}

// OpenRouter says what each call cost, which is what's charged.
func TestOpenRoutersOwnCostIsWhatsCharged(t *testing.T) {
	fake, url := newFakeProvider(t, completion("Green.", `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cost":0.0123}`))
	res, err := brainWith(models.OpenRouter, url).Respond(context.Background(), wake("openrouter:mistralai/mistral-large-2607", ""), nil)

	req, _, _ := fake.last(t)
	if err != nil || req["model"] != "mistralai/mistral-large-2607" || res.Cost != 0.0123 {
		t.Errorf("model %v, cost %v: %v", req["model"], res.Cost, err)
	}
}

// A provider that doesn't say what a call used is charged for an estimate,
// never for nothing.
func TestACallWithoutUsageIsChargedAnEstimate(t *testing.T) {
	_, url := newFakeProvider(t, completion(strings.Repeat("word ", 200), `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`))
	res, err := brainWith(models.OpenAI, url).Respond(context.Background(), wake("openai:gpt-6-luna", ""), nil)
	if err != nil || res.Usage.Input == 0 || res.Usage.Output < 200 {
		t.Errorf("usage %+v, %v; want an estimate from what was sent and said", res.Usage, err)
	}
}

func TestARefusalIsAnError(t *testing.T) {
	_, url := newFakeProvider(t, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop",
		"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	if _, err := brainWith(models.OpenAI, url).Respond(context.Background(), wake("openai:gpt-6-sol", ""), nil); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("err = %v, want the model declining", err)
	}
}

// A model whose provider the network has no key for says so.
func TestAModelWithoutItsProvidersKeySaysSo(t *testing.T) {
	_, url := newFakeProvider(t, completion("hi", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	_, err := brainWith(models.OpenAI, url).Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)
	if err == nil || !strings.Contains(err.Error(), "Google Gemini") {
		t.Errorf("err = %v, want one naming the provider without a key", err)
	}
}

// A runner's keys come from the platform's broker, asked with its network's
// secret, and can change while it runs: a new key is used from the next call.
func TestKeysComeFromTheBrokerAndCanChange(t *testing.T) {
	var mu sync.Mutex
	keys := `{"keys":{"openai":"key-from-broker"}}`
	var asked []string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("GuildNet-Network")+" "+r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer net-secret" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		io.WriteString(w, keys)
	}))
	defer broker.Close()
	fake, url := newFakeProvider(t, completion("Green.", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	b := brain.New(brain.Options{BaseURLs: map[models.Provider]string{models.OpenAI: url}})
	if _, err := b.Respond(context.Background(), wake("openai:gpt-6-sol", ""), nil); err == nil {
		t.Fatal("answered with no keys")
	}

	got, err := (brain.Broker{URL: broker.URL, Network: "acme", Secret: "net-secret"}).Keys(context.Background())
	if err != nil || got[models.OpenAI] != "key-from-broker" {
		t.Fatalf("keys %v, %v", got, err)
	}
	b.SetKeys(got)
	if _, err := b.Respond(context.Background(), wake("openai:gpt-6-sol", ""), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, auth := fake.last(t); auth != "Bearer key-from-broker" {
		t.Errorf("OpenAI was asked with %q", auth)
	}
	if p := b.Providers(); len(p) != 1 || p[0] != models.OpenAI {
		t.Errorf("providers %v", p)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != "GET /model-keys acme Bearer net-secret" {
		t.Errorf("the broker was asked %q", asked)
	}
	mu.Unlock()

	b.SetKeys(map[models.Provider]string{models.OpenAI: "key-replaced"})
	b.Respond(context.Background(), wake("openai:gpt-6-sol", ""), nil)
	if _, _, auth := fake.last(t); auth != "Bearer key-replaced" {
		t.Errorf("after a new key, OpenAI was asked with %q", auth)
	}

	_, err = (brain.Broker{URL: broker.URL, Network: "acme", Secret: "wrong"}).Keys(context.Background())
	if err == nil || strings.Contains(err.Error(), "key-") {
		t.Errorf("a refused ask: %v", err)
	}
}

// On the VM the broker is reached over TLS, its certificate from GuildNet's
// own CA, which is all the runner trusts.
func TestTheBrokerIsReachedOverTLSFromOurCA(t *testing.T) {
	dir, certs := t.TempDir(), t.TempDir()
	ca, _ := pki.EnsureCA(dir)
	ca.Issue(certs, "broker", []string{"localhost"}, time.Now())
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(certs, "broker.crt"), filepath.Join(certs, "broker.key")))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"keys":{"anthropic":"key-over-tls"}}`)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	url := "https://localhost:" + port

	cfg, _ := pki.ClientTLS(ca.CertPEM())
	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	keys, err := (brain.Broker{URL: url, Network: "acme", Secret: "s", HTTP: trusting}).Keys(context.Background())
	if err != nil || keys[models.Anthropic] != "key-over-tls" {
		t.Errorf("over TLS: %v, %v", keys, err)
	}
	if _, err := (brain.Broker{URL: url, Network: "acme", Secret: "s"}).Keys(context.Background()); err == nil {
		t.Error("a client not trusting our CA got keys")
	}
}

// Claude models keep Claude's own API. Effort goes only to models that take
// it: Haiku 4.5 doesn't.
func TestClaudeGetsEffortOnlyWhereItTakesIt(t *testing.T) {
	fake, url := newFakeProvider(t, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Green."}],
		"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3}}`)
	b := brainWith(models.Anthropic, strings.TrimSuffix(url, "v1/"))

	res, err := b.Respond(context.Background(), wake("claude-haiku-4-5", "medium"), nil)
	req, path, auth := fake.last(t)
	if err != nil || res.Reply != "Green." || path != "/v1/messages" || auth != "key-anthropic" || req["model"] != "claude-haiku-4-5" {
		t.Fatalf("asked %s with %q for %v: %v", path, auth, req["model"], err)
	}
	if _, sent := req["output_config"]; sent {
		t.Errorf("Haiku was sent an effort: %v", req["output_config"])
	}
	if res.Usage != (ledger.Usage{Input: 10, Output: 5, CacheRead: 3}) {
		t.Errorf("usage %+v", res.Usage)
	}
	b.Respond(context.Background(), wake("claude-sonnet-5-5", "high"), nil)
	if req, _, _ := fake.last(t); req["output_config"] == nil {
		t.Error("Sonnet 5.5 takes an effort, and wasn't sent one")
	}
}

// Picking who answers, and catch-ups, run on whatever model they're given,
// through its provider.
func TestSmallCallsGoThroughTheirModelsProvider(t *testing.T) {
	fake, url := newFakeProvider(t, completion("kai", `{"prompt_tokens":50,"completion_tokens":2,"total_tokens":52}`))
	b := brainWith(models.OpenAI, url)
	picked, u, err := b.PickResponders(context.Background(), "openai:gpt-6-luna", "#engineering", "", nil,
		[]config.Persona{{Nick: "kai", Role: "Backend engineer"}})
	if err != nil || len(picked) != 1 || picked[0] != "kai" || u.Input != 50 {
		t.Errorf("picked %v, usage %+v: %v", picked, u, err)
	}
	fake.setBody(completion("For you: nothing waiting on you.", `{"prompt_tokens":80,"completion_tokens":9,"total_tokens":89}`))
	summary, _, err := b.Catchup(context.Background(), agent.CatchupRequest{Model: "openai:gpt-6-luna", For: "method", Since: time.Now().Add(-time.Hour)})
	if req, _, _ := fake.last(t); err != nil || !strings.HasPrefix(summary, "For you") || req["model"] != "gpt-6-luna" {
		t.Errorf("catch-up %q via %v: %v", summary, req["model"], err)
	}
}
