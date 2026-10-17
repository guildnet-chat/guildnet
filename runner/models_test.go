// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/models"
)

// A network runs its agents on the model providers it has keys for: new
// hires and presets start on those providers' models, and an agent can only
// be moved to a model one of them serves.

func withProviders(ps ...models.Provider) option {
	return func(cfg *config.Runner, _ *[]config.Persona) { cfg.Providers = ps }
}

func modelOf(t *testing.T, n *network, nick string) string {
	t.Helper()
	_, out := n.api("method").call("GET", "/v1/staff/"+nick, nil)
	m, _ := out["model"].(string)
	return m
}

func TestNewHiresStartOnTheirProvidersModels(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withProviders(models.OpenAI))

	n.human.say("GuildServ", "HIRE rex")
	n.human.expect("rex hired", guildServSaying("method", "rex"))
	n.human.say("GuildServ", "HIRE sue PRESET support-agent")
	n.human.expect("sue hired", guildServSaying("method", "sue"))

	if got := modelOf(t, n, "rex"); got != "openai:gpt-6-sol" {
		t.Errorf("rex starts on %q, want OpenAI's balanced model", got)
	}
	if got := modelOf(t, n, "sue"); got != "openai:gpt-6-luna" {
		t.Errorf("sue (a support agent, a fast role) starts on %q, want OpenAI's fast model", got)
	}
}

func TestAnAgentCanOnlyMoveToAModelItsNetworkHasAKeyFor(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withProviders(models.OpenAI))

	n.human.say("GuildServ", "SET kai MODEL claude-opus-5-5")
	n.human.expect("no Anthropic key", guildServSaying("method", "no Anthropic key"))
	n.human.say("GuildServ", "SET kai MODEL gpt-9000")
	n.human.expect("not a model", guildServSaying("method", "isn't a model"))
	n.human.say("GuildServ", "SET kai MODEL best")
	n.human.expect("kai on the best tier", guildServSaying("method", "openai:gpt-6-astra"))

	if got := modelOf(t, n, "kai"); got != "openai:gpt-6-astra" {
		t.Errorf("kai is on %q, want the best tier's model", got)
	}
	expectStatus(t, n, "method", http.StatusBadRequest, "PATCH", "/v1/staff/kai", map[string]any{"model": "google:gemini-3.8-flash"})
}

// The platform asks the runner to fetch its model keys again when they
// change, so a new key works at once.
func TestTheRunnerFetchesItsKeysAgainWhenAsked(t *testing.T) {
	var mu sync.Mutex
	fetched := 0
	n := startNetwork(t, &fakeBrain{}, withAPI(), func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.RefreshModelKeys = func() { mu.Lock(); fetched++; mu.Unlock() }
	})

	status, _ := n.api("method").call("POST", "/v1/model-keys/refresh", nil)

	mu.Lock()
	defer mu.Unlock()
	if status != http.StatusNoContent || fetched != 1 {
		t.Errorf("status %d, fetched %d times", status, fetched)
	}
}

// Which providers a network has can change while it runs (keys fetched from
// the platform), and so does what each tier means.
func TestTiersFollowTheProvidersANetworkHasNow(t *testing.T) {
	have := []models.Provider{models.Anthropic}
	var mu sync.Mutex
	n := startNetwork(t, &fakeBrain{}, withAPI(), func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.ProvidersLive = func() []models.Provider { mu.Lock(); defer mu.Unlock(); return have }
	})
	n.human.say("GuildServ", "SET kai MODEL fast")
	n.human.expect("kai on Haiku", guildServSaying("method", "claude-haiku-5-5"))

	mu.Lock()
	have = []models.Provider{models.OpenAI}
	mu.Unlock()
	n.human.say("GuildServ", "SET kai MODEL fast")
	n.human.expect("kai on GPT-6 Luna", guildServSaying("method", "openai:gpt-6-luna"))
}

// The console offers the models a network's providers serve, cheapest tier
// first, each with its tier and price, and says which one a new hire gets.
func TestTheConsoleIsOfferedTheModelsItsProvidersServe(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withProviders(models.OpenAI))

	_, out := n.api("method").call("GET", "/v1/models", nil)

	var ids []string
	for _, x := range out["models"].([]any) {
		m := x.(map[string]any)
		ids = append(ids, m["id"].(string))
		if m["id"] == "openai:gpt-6-luna" && (m["title"] != "GPT-6 Luna" || m["tier"] != "fast" || m["pricing"] != "$0.10 in, $0.50 out per million tokens") {
			t.Errorf("GPT-6 Luna is offered as %v", m)
		}
	}
	if want := []string{"openai:gpt-6-luna", "openai:gpt-6-sol", "openai:gpt-6-astra"}; !slices.Equal(ids, want) {
		t.Errorf("offered %v, want %v", ids, want)
	}
	if out["default"] != "openai:gpt-6-sol" {
		t.Errorf("a new hire gets %v, want the balanced tier's model", out["default"])
	}
}

// An agent's model is shown with what it costs, in IRC and the console,
// even an older one no longer offered for new hires.
func TestAnAgentsModelIsShownWithItsPrice(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	n.human.say("GuildServ", "INFO kai")
	n.human.expect("kai's model and price", guildServSaying("method", "Model Claude Sonnet 5 (claude-sonnet-5; $2 in, $10 out per million tokens)"))

	_, out := n.api("method").call("GET", "/v1/staff/kai", nil)
	if out["model_title"] != "Claude Sonnet 5" || out["model_pricing"] != "$2 in, $10 out per million tokens" {
		t.Errorf("kai's model is shown as %v, %v", out["model_title"], out["model_pricing"])
	}
}

// The role catalogue says which model each role would start on here, so the
// console can show it.
func TestTheRoleCatalogueShowsEachRolesModelHere(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withProviders(models.Google))
	_, out := n.api("method").call("GET", "/v1/roles", nil)
	for _, x := range out["roles"].([]any) {
		r := x.(map[string]any)
		if r["key"] == "support-agent" && (r["tier"] != "fast" || r["model"] != "google:gemini-3.5-flash-lite") {
			t.Errorf("support agent: tier %v, model %v; want fast, Gemini's fast model", r["tier"], r["model"])
		}
	}
}
