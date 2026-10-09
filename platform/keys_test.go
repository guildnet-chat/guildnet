// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network's agents run on the model providers its owner gives keys for:
// any of them, at sign-up and in the console's settings. Each key is checked
// with its provider when it's given, and never shown again.

// A network's runner gets its model keys from the platform's broker (on the
// private networks only), with its network's secret: its own keys, never
// another network's, and nothing kept along the way.
func TestTheBrokerGivesARunnerItsOwnModelKeysOnly(t *testing.T) {
	w := NewWorld(t)
	w.Created("ada@example.com", "initech")
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	secret := func(name string) string {
		n, _ := w.Store.Network(context.Background(), name)
		return n.RunnerToken
	}
	ask := func(network, token string) (int, http.Header, map[string]map[string]string) {
		req, _ := http.NewRequest("GET", broker.URL+"/model-keys", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("GuildNet-Network", network)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, resp.Header, out
	}

	status, header, out := ask("initech", secret("initech"))
	if status != http.StatusOK || out["keys"]["anthropic"] != "sk-ant-api03-test-key-0123456789" || len(out["keys"]) != 1 {
		t.Errorf("initech's runner asking: %d %v", status, out)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q; keys mustn't be kept along the way", header.Get("Cache-Control"))
	}
	for _, bad := range [][2]string{{"initech", "not-the-secret"}, {"initech", ""}, {"acme", secret("initech")}, {"nowhere", secret("initech")}} {
		if status, _, out := ask(bad[0], bad[1]); status != http.StatusUnauthorized || len(out["keys"]) > 0 {
			t.Errorf("asking for %s with the wrong secret: %d %v", bad[0], status, out)
		}
	}
}

// Where runners fetch their keys from the platform's broker, the host is
// never sent them: not at creation, nor when one changes.
func TestWithABrokerTheHostIsNeverSentKeys(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.KeysByBroker = true })
	_, console := w.Created("ada@example.com", "initech")
	console.Get("/settings")
	console.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-initech-0123456789"}})

	if s, _ := w.Host.Spec("initech"); len(s.ModelKeys) != 0 || s.AnthropicKey != "" {
		t.Errorf("the host was sent keys: %d", len(s.ModelKeys))
	}
	if n, _ := w.Store.Network(context.Background(), "initech"); n.Keys.OpenAI != "sk-proj-initech-0123456789" || n.Keys.Anthropic == "" {
		t.Error("the platform should still keep them")
	}
}

// Changing a key in settings has the runner fetch its keys again, so the
// change takes effect at once.
func TestChangingAKeyHasTheRunnerFetchItsKeys(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	before := w.Host.KeyRefreshes("initech")
	console.Get("/settings")
	console.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-initech-0123456789"}})
	if w.Host.KeyRefreshes("initech") <= before {
		t.Error("the runner wasn't asked to fetch its keys again")
	}
}

func TestANetworkCanStartWithAnyProvidersKey(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	if form := b.Get("/networks/new"); !strings.Contains(form.Text(), "OpenAI") || !strings.Contains(form.Text(), "OpenRouter") {
		t.Errorf("the create page doesn't offer each provider:\n%s", form.Text())
	}
	f := CreateForm("initech")
	f.Set("provider", "openai")
	f.Set("model_key", "sk-proj-initech-0123456789")
	if r := b.Post("/networks", f); r.Status != http.StatusSeeOther {
		t.Fatalf("creating: %d %s", r.Status, r.Text())
	}
	b.WaitReady("initech")

	if s, _ := w.Host.Spec("initech"); len(s.ModelKeys) != 1 || s.ModelKeys[models.OpenAI] != "sk-proj-initech-0123456789" {
		t.Errorf("the host was given keys %v", s.ModelKeys)
	}
}

func TestAKeyItsProviderRefusesIsntTaken(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	f := CreateForm("initech")
	f.Set("provider", "google")
	f.Set("model_key", "AIzaSy-refused-0123456789")

	r := b.Post("/networks", f)

	if r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "Google Gemini didn't accept that key") {
		t.Errorf("%d:\n%s", r.Status, r.Text())
	}
	if strings.Contains(r.Body, "AIzaSy-refused") {
		t.Error("the page repeats the key back")
	}
}

// An API key isn't a password: browsers mustn't offer to save it, fill it
// in, or pop a password manager over the box, on sign-up or in settings. It
// still doesn't show while it's typed.
func TestPasswordManagersLeaveTheKeyBoxAlone(t *testing.T) {
	w := NewWorld(t)
	signUp := w.OnHome("ada@example.com").Get("/networks/new").Body
	_, console := w.Created("ada@example.com", "initech")
	box := regexp.MustCompile(`<input [^>]*name="model_key"[^>]*>`)
	for name, page := range map[string]string{"signing up": signUp, "settings": console.Get("/settings").Body} {
		input := box.FindString(page)
		if input == "" {
			t.Fatalf("%s has no key box", name)
		}
		for _, want := range []string{`type="text"`, `autocomplete="off"`, `class="secret"`, `data-lpignore="true"`, `data-1p-ignore`, `data-bwignore`, `data-form-type="other"`} {
			if !strings.Contains(input, want) {
				t.Errorf("%s's key box %s should have %s", name, input, want)
			}
		}
	}
	css := w.Home().Get("/static/tokens.css").Body
	if !strings.Contains(css, "-webkit-text-security: disc") {
		t.Error("a key shouldn't show while it's typed")
	}
}

// Google AI Studio's keys have started with "AQ." since May 2026; older
// ones start with "AIza". Both are Gemini keys.
func TestBothKindsOfGeminiKeyAreTaken(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	for _, key := range []string{"AQ.Ab8RN6-initech_0123456789.abcdef", "AIzaSy-initech-0123456789"} {
		console.Get("/settings")
		if r := console.Post("/settings/keys", url.Values{"provider": {"google"}, "model_key": {key}}); r.Status != http.StatusSeeOther {
			t.Errorf("a Gemini key starting %s: %d %s", key[:4], r.Status, r.Text())
		}
	}
	for _, bad := range []string{"AQ.", "AQ..0123456789abcdef0123", "AQ.has spaces 0123456789abcdef"} {
		console.Get("/settings")
		if r := console.Post("/settings/keys", url.Values{"provider": {"google"}, "model_key": {bad}}); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "AQ.") {
			t.Errorf("%q: %d, want refused with a hint naming AQ.", bad, r.Status)
		}
	}
}

func TestTheOwnerManagesTheModelKeysWithoutSeeingThem(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	page := console.Get("/settings")
	if strings.Contains(page.Body, "sk-ant-api03-test-key-0123456789") || !strings.Contains(page.Text(), "6789") {
		t.Errorf("the page should show only the key's last four characters:\n%s", page.Text())
	}
	keys := func() map[models.Provider]string {
		s, _ := w.Host.Spec("initech")
		return s.ModelKeys
	}

	// Adding Gemini keeps Anthropic.
	if r := console.Post("/settings/keys", url.Values{"provider": {"google"}, "model_key": {"AIzaSy-initech-0123456789"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("adding Gemini: %d %s", r.Status, r.Text())
	}
	if k := keys(); k[models.Google] != "AIzaSy-initech-0123456789" || k[models.Anthropic] != "sk-ant-api03-test-key-0123456789" {
		t.Errorf("after adding Gemini: %v", k)
	}
	// Replacing Anthropic's.
	console.Get("/settings")
	console.Post("/settings/keys", url.Values{"provider": {"anthropic"}, "model_key": {"sk-ant-api03-new-key-9876543210"}})
	if k := keys(); k[models.Anthropic] != "sk-ant-api03-new-key-9876543210" {
		t.Errorf("after replacing Anthropic's: %v", k)
	}
	// One that isn't a key, or that its provider refuses, changes nothing.
	for _, bad := range []url.Values{
		{"provider": {"openai"}, "model_key": {"not a key"}},
		{"provider": {"openai"}, "model_key": {"sk-proj-refused-0123456789"}},
		{"provider": {"skynet"}, "model_key": {"sk-0123456789abcdef"}},
	} {
		console.Get("/settings")
		if r := console.Post("/settings/keys", bad); r.Status != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, r.Status)
		}
	}
	// Removing one; never the last.
	console.Get("/settings")
	if r := console.Post("/settings/keys/remove", url.Values{"provider": {"anthropic"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("removing Anthropic's: %d %s", r.Status, r.Text())
	}
	if k := keys(); len(k) != 1 || k[models.Google] == "" {
		t.Errorf("after removing Anthropic's: %v", k)
	}
	console.Get("/settings")
	if r := console.Post("/settings/keys/remove", url.Values{"provider": {"google"}}); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "at least one") {
		t.Errorf("removing the last key: %d %s", r.Status, r.Text())
	}
	if len(keys()) != 1 {
		t.Error("the last key went")
	}
}
