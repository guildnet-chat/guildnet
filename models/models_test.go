// SPDX-License-Identifier: AGPL-3.0-only

package models_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/models"
)

// Models are named with their provider ("openai:gpt-6-sol"); Claude's keep
// their bare names, which is how every existing agent already names them.
func TestModelsAreFoundByTheirProviderAndName(t *testing.T) {
	for id, want := range map[string]struct {
		id       string
		provider models.Provider
		name     string
	}{
		"claude-sonnet-5-5":                      {"claude-sonnet-5-5", models.Anthropic, "claude-sonnet-5-5"},
		"anthropic:claude-sonnet-5-5":            {"claude-sonnet-5-5", models.Anthropic, "claude-sonnet-5-5"},
		"claude-sonnet-5":                        {"claude-sonnet-5", models.Anthropic, "claude-sonnet-5"},
		"claude-haiku-4-5":                       {"claude-haiku-4-5", models.Anthropic, "claude-haiku-4-5"},
		"openai:gpt-6-sol":                       {"openai:gpt-6-sol", models.OpenAI, "gpt-6-sol"},
		" OpenAI:GPT-6-Sol ":                     {"openai:gpt-6-sol", models.OpenAI, "gpt-6-sol"},
		"google:gemini-3.8-flash":                {"google:gemini-3.8-flash", models.Google, "gemini-3.8-flash"},
		"openrouter:anthropic/claude-sonnet-5.5": {"openrouter:anthropic/claude-sonnet-5.5", models.OpenRouter, "anthropic/claude-sonnet-5.5"},
	} {
		m, ok := models.Find(id)
		if !ok || m.ID != want.id || m.Provider != want.provider || m.Name != want.name {
			t.Errorf("Find(%q) = %+v, %v; want %+v", id, m, ok, want)
		}
	}
	for _, id := range []string{"", "gpt-6-sol", "openai:gpt-9000", "claude-nonsense", "mystery:model", "anthropic:", "openrouter:", "openrouter:no-vendor", "openrouter:a/b c"} {
		if m, ok := models.Find(id); ok {
			t.Errorf("Find(%q) = %+v; it isn't a model we know", id, m)
		}
	}
}

// OpenRouter serves hundreds of models and says what each call cost, so any
// of its models can be used, priced by what it reports.
func TestAnyOpenRouterModelCanBeUsedAndIsPricedByOpenRouter(t *testing.T) {
	m, ok := models.Find("openrouter:mistralai/mistral-large-2607")
	if !ok || m.Provider != models.OpenRouter || m.Name != "mistralai/mistral-large-2607" || m.Priced {
		t.Errorf("an OpenRouter model we don't list: %+v, %v; want usable, priced by OpenRouter", m, ok)
	}
	if listed, _ := models.Find("openrouter:openai/gpt-6-sol"); !listed.Priced || listed.Price(time.Now()).Input != 2 {
		t.Errorf("an OpenRouter model we list keeps its price: %+v", listed)
	}
}

// Presets name a tier, and each tier comes from the providers a company has,
// preferring Anthropic, then OpenAI, then Google, then OpenRouter; a
// provider without a model for a tier gives its next best.
func TestEachTierComesFromTheProvidersACompanyHas(t *testing.T) {
	for _, c := range []struct {
		have []models.Provider
		tier models.Tier
		want string
	}{
		{[]models.Provider{models.Anthropic}, models.Fast, "claude-haiku-4-5"},
		{[]models.Provider{models.Anthropic}, models.Balanced, "claude-sonnet-5-5"},
		{[]models.Provider{models.Anthropic}, models.Best, "claude-opus-5-5"},
		{[]models.Provider{models.OpenAI}, models.Balanced, "openai:gpt-6-sol"},
		{[]models.Provider{models.OpenAI, models.Anthropic}, models.Balanced, "claude-sonnet-5-5"},
		{[]models.Provider{models.Google}, models.Fast, "google:gemini-3.5-flash-lite"},
		{[]models.Provider{models.Google}, models.Best, "google:gemini-3.8-flash"},
		{[]models.Provider{models.OpenRouter}, models.Balanced, "openrouter:anthropic/claude-sonnet-5.5"},
	} {
		m, ok := models.ForTier(c.tier, c.have)
		if !ok || m.ID != c.want {
			t.Errorf("%s from %v = %q, %v; want %s", c.tier, c.have, m.ID, ok, c.want)
		}
	}
	if m, ok := models.ForTier(models.Balanced, nil); ok {
		t.Errorf("with no providers there's no model, got %q", m.ID)
	}
}

// What's offered when hiring: current models from the company's providers,
// not older ones kept only for agents already on them.
func TestOnlyCurrentModelsFromYourProvidersAreOffered(t *testing.T) {
	offered := map[string]bool{}
	for _, m := range models.Offered([]models.Provider{models.OpenAI}) {
		offered[m.ID] = true
		if m.Provider != models.OpenAI {
			t.Errorf("%s offered to a company with only an OpenAI key", m.ID)
		}
	}
	if !offered["openai:gpt-6-sol"] || !offered["openai:gpt-6-luna"] || offered["openai:gpt-5.6-sol"] {
		t.Errorf("offered: %v", offered)
	}
	for _, m := range models.Offered([]models.Provider{models.Anthropic}) {
		if m.ID == "claude-sonnet-5" || m.ID == "claude-opus-5" {
			t.Errorf("%s is older; kept for agents on it, not offered", m.ID)
		}
	}
}

// Prices are per million tokens, and some are promotional: Gemini 3.8 Flash
// doubles in 2027.
func TestPricesChangeOnTheDayTheProviderSaid(t *testing.T) {
	m, _ := models.Find("google:gemini-3.8-flash")
	before := m.Price(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC))
	after := m.Price(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if before.Input != 0.75 || after.Input != 1.50 || after.Output != 7.50 {
		t.Errorf("before %+v, after %+v", before, after)
	}
	if opus, _ := models.Find("claude-opus-5-5"); opus.Price(time.Now()) != (models.Price{Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}) {
		t.Errorf("Opus 5.5 = %+v", opus.Price(time.Now()))
	}
}

// People see what a model costs in words, in whole dollars where they can;
// an OpenRouter model we don't list is priced by OpenRouter as it's used.
func TestAModelsPriceIsSaidPlainly(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	for id, want := range map[string]string{
		"claude-sonnet-5-5":                    "$2 in, $10 out per million tokens",
		"openai:gpt-6-luna":                    "$0.10 in, $0.50 out per million tokens",
		"google:gemini-3.8-flash":              "$0.75 in, $3.75 out per million tokens",
		"openrouter:mistralai/mistral-large-2": "priced by OpenRouter as it's used",
	} {
		m, _ := models.Find(id)
		if got := m.Pricing(now); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}

// Wherever a model is named (a persona file, MODEL, a preset), a tier works
// too, and a model is only usable if the company has its provider's key.
func TestAModelOrTierResolvesAgainstTheCompanysProviders(t *testing.T) {
	openai := []models.Provider{models.OpenAI}
	if m, err := models.Resolve("balanced", openai); err != nil || m.ID != "openai:gpt-6-sol" {
		t.Errorf("balanced with OpenAI = %q, %v", m.ID, err)
	}
	if m, err := models.Resolve("openai:gpt-6-luna", openai); err != nil || m.ID != "openai:gpt-6-luna" {
		t.Errorf("a model of theirs = %q, %v", m.ID, err)
	}
	_, err := models.Resolve("claude-sonnet-5-5", openai)
	if !errors.Is(err, models.ErrNoKey) || !strings.Contains(err.Error(), "Anthropic") {
		t.Errorf("a model without its provider's key: %v; want ErrNoKey naming Anthropic", err)
	}
	if _, err := models.Resolve("gpt-9000", openai); !errors.Is(err, models.ErrUnknown) {
		t.Errorf("an unknown model: %v, want ErrUnknown", err)
	}
	if _, err := models.Resolve("fast", nil); !errors.Is(err, models.ErrNoKey) {
		t.Errorf("a tier with no providers at all: %v, want ErrNoKey", err)
	}
}

// Providers are only ever reached at their own fixed addresses, over HTTPS:
// never an address a customer could point somewhere inside our network.
func TestProvidersAreReachedAtTheirOwnAddresses(t *testing.T) {
	want := map[models.Provider]string{
		models.Anthropic:  "",
		models.OpenAI:     "https://api.openai.com/v1/",
		models.Google:     "https://generativelanguage.googleapis.com/v1beta/openai/",
		models.OpenRouter: "https://openrouter.ai/api/v1/",
	}
	for _, p := range models.Providers {
		if p.BaseURL() != want[p] {
			t.Errorf("%s is reached at %q, want %q", p, p.BaseURL(), want[p])
		}
	}
}

// Each provider's key reaches the runner in its own environment variable.
func TestEachProvidersKeyHasItsOwnVariable(t *testing.T) {
	want := map[models.Provider]string{models.Anthropic: "ANTHROPIC_API_KEY", models.OpenAI: "OPENAI_API_KEY", models.Google: "GEMINI_API_KEY", models.OpenRouter: "OPENROUTER_API_KEY"}
	for _, p := range models.Providers {
		if p.KeyVar() != want[p] {
			t.Errorf("%s's key is in %q, want %q", p, p.KeyVar(), want[p])
		}
	}
}

func TestEveryModelIsComplete(t *testing.T) {
	for _, m := range models.All {
		p := m.Price(time.Now())
		if m.ID == "" || m.Name == "" || m.Title == "" || !m.Priced || p.Input <= 0 || p.Output <= 0 {
			t.Errorf("incomplete: %+v", m)
		}
		if again, ok := models.Find(m.ID); !ok || again.ID != m.ID {
			t.Errorf("%s isn't found by its own id", m.ID)
		}
	}
}
