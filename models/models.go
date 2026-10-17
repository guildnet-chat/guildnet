// SPDX-License-Identifier: AGPL-3.0-only

// Package models is GuildNet's model catalogue: the models agents can run
// on, from each provider a company can connect, with what they cost and how
// good they are (a tier), so the org's role presets work whatever providers a
// company has, and spend is counted right.
//
// A model is named with its provider, "openai:gpt-6-sol"; Claude models keep
// their bare names ("claude-sonnet-5-5"), as agents have always named them.
// Prices are the providers' list prices, checked 2026-09-29 (Anthropic's and
// OpenAI's pricing pages, Google's, and OpenRouter's /models).
package models

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Version is when the catalogue's prices were last checked; each ledger
// entry keeps it, so old spend can be explained after prices change.
const Version = "2026-09-29"

// Provider is who serves a model, with the key a company gives us.
type Provider string

const (
	Anthropic  Provider = "anthropic"
	OpenAI     Provider = "openai"
	Google     Provider = "google"
	OpenRouter Provider = "openrouter"
)

// Providers are in order of preference, for picking a tier's model.
var Providers = []Provider{Anthropic, OpenAI, Google, OpenRouter}

// Title is the provider's name, as people know it.
func (p Provider) Title() string {
	switch p {
	case Anthropic:
		return "Anthropic"
	case OpenAI:
		return "OpenAI"
	case Google:
		return "Google Gemini"
	case OpenRouter:
		return "OpenRouter"
	}
	return string(p)
}

// Tier is how capable (and costly) a model is, which is what role presets
// ask for.
type Tier string

const (
	Fast     Tier = "fast"
	Balanced Tier = "balanced"
	Best     Tier = "best"
)

// Title is the tier's name for people: "Fast".
func (t Tier) Title() string {
	if t == "" {
		return ""
	}
	return strings.ToUpper(string(t[:1])) + string(t[1:])
}

// Tiers, cheapest first.
var Tiers = []Tier{Fast, Balanced, Best}

// Price is USD per million tokens.
type Price struct {
	Input, Output, CacheRead, CacheWrite float64
}

// Change is a price the provider has said a model moves to, from a date.
type Change struct {
	From  time.Time
	Price Price
}

// Long is a model's price for a call whose prompt (input, cache reads and
// cache writes together) is over Over tokens.
type Long struct {
	Over  int64
	Price Price
}

// Model is one model an agent can run on.
type Model struct {
	ID       string   // how GuildNet names it: "claude-sonnet-5-5", "openai:gpt-6-sol"
	Provider Provider // who serves it
	Name     string   // the provider's own name for it
	Title    string   // for people: "Claude Sonnet 5.5"
	// Tier is what it's picked for; Top marks the tier's usual pick at its
	// provider; Older ones are kept for agents already on them but not
	// offered when hiring.
	Tier  Tier
	Top   bool
	Older bool
	// Priced says the catalogue knows its price; if not (an OpenRouter model
	// we don't list), the provider reports what each call cost.
	Priced bool
	price  Price
	later  []Change
	long   *Long // its price for a long prompt, if it has one
	// Effort says it takes an effort (reasoning) setting.
	Effort bool
	// openRouter is its name on OpenRouter, where it's served too.
	openRouter string
}

// Price is what it costs at t.
func (m Model) Price(t time.Time) Price {
	p := m.price
	for _, c := range m.later {
		if !t.Before(c.From) {
			p = c.Price
		}
	}
	return p
}

// PriceFor is what a call with a prompt of prompt tokens (input, cache
// reads and cache writes together) costs at t: its long-prompt price past
// the length the provider prices by, its price otherwise.
func (m Model) PriceFor(t time.Time, prompt int64) Price {
	if m.long != nil && prompt > m.long.Over {
		return m.long.Price
	}
	return m.Price(t)
}

// Pricing is what it costs at t, for people: "$2 in, $10 out per million
// tokens".
func (m Model) Pricing(t time.Time) string {
	if !m.Priced {
		return "priced by " + m.Provider.Title() + " as it's used"
	}
	p := m.Price(t)
	s := dollars(p.Input) + " in, " + dollars(p.Output) + " out per million tokens"
	if m.long != nil {
		s += fmt.Sprintf(" (%s in, %s out for prompts over %s tokens)", dollars(m.long.Price.Input), dollars(m.long.Price.Output), thousands(m.long.Over))
	}
	return s
}

// thousands is n with commas: "100,000".
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// dollars is "$2" for a whole number, "$0.10" otherwise.
func dollars(v float64) string {
	return "$" + strings.TrimSuffix(fmt.Sprintf("%.2f", v), ".00")
}

var newYear2027 = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

// listed is the catalogue as providers serve it; OpenRouter's copies are
// added from it.
var listed = []Model{
	// Anthropic. Cache reads are 10% of input, except Opus 5.5 (5%) and
	// Fable 5.1 (2.5%); writes are 125%. Haiku 5.5's prompts over 100,000
	// tokens cost five times its prices (checked 2026-10-17).
	{ID: "claude-haiku-5-5", Name: "claude-haiku-5-5", Title: "Claude Haiku 5.5", Tier: Fast, Top: true, Effort: true,
		price: Price{0.10, 0.50, 0.01, 0.125}, long: &Long{100_000, Price{0.50, 2.50, 0.05, 0.625}}, openRouter: "anthropic/claude-haiku-5.5"},
	{ID: "claude-haiku-4-5", Name: "claude-haiku-4-5", Title: "Claude Haiku 4.5", Tier: Fast, Older: true,
		price: Price{1, 5, 0.1, 1.25}, openRouter: "anthropic/claude-haiku-4.5"},
	{ID: "claude-sonnet-5-5", Name: "claude-sonnet-5-5", Title: "Claude Sonnet 5.5", Tier: Balanced, Top: true, Effort: true,
		price: Price{2, 10, 0.2, 2.5}, openRouter: "anthropic/claude-sonnet-5.5"},
	{ID: "claude-opus-5-5", Name: "claude-opus-5-5", Title: "Claude Opus 5.5", Tier: Best, Top: true, Effort: true,
		price: Price{4, 20, 0.2, 5}, openRouter: "anthropic/claude-opus-5.5"},
	{ID: "claude-fable-5-1", Name: "claude-fable-5-1", Title: "Claude Fable 5.1", Tier: Best, Effort: true,
		price: Price{10, 50, 0.25, 12.5}, openRouter: "anthropic/claude-fable-5.1"},
	{ID: "claude-sonnet-5", Name: "claude-sonnet-5", Title: "Claude Sonnet 5", Tier: Balanced, Older: true, Effort: true,
		price: Price{2, 10, 0.2, 2.5}, openRouter: "anthropic/claude-sonnet-5"},
	{ID: "claude-opus-5", Name: "claude-opus-5", Title: "Claude Opus 5", Tier: Best, Older: true, Effort: true,
		price: Price{5, 25, 0.5, 6.25}, openRouter: "anthropic/claude-opus-5"},

	// OpenAI. Cached input is 10% of input.
	{ID: "openai:gpt-6-luna", Name: "gpt-6-luna", Title: "GPT-6 Luna", Tier: Fast, Top: true, Effort: true,
		price: Price{0.10, 0.50, 0.01, 0.125}, openRouter: "openai/gpt-6-luna"},
	{ID: "openai:gpt-6-sol", Name: "gpt-6-sol", Title: "GPT-6 Sol", Tier: Balanced, Top: true, Effort: true,
		price: Price{2, 10, 0.2, 2.5}, openRouter: "openai/gpt-6-sol"},
	{ID: "openai:gpt-6-astra", Name: "gpt-6-astra", Title: "GPT-6 Astra", Tier: Best, Top: true, Effort: true,
		price: Price{10, 50, 1, 12.5}, openRouter: "openai/gpt-6-astra"},
	{ID: "openai:gpt-5.6-sol", Name: "gpt-5.6-sol", Title: "GPT-5.6 Sol", Tier: Best, Older: true, Effort: true,
		price: Price{4, 20, 0.4, 5}, openRouter: "openai/gpt-5.6-sol"},

	// Google. No Gemini Pro is generally available, so Flash is its best.
	// Gemini 3.8 Flash's price is an introductory one: it doubles in 2027.
	{ID: "google:gemini-3.5-flash-lite", Name: "gemini-3.5-flash-lite", Title: "Gemini 3.5 Flash-Lite", Tier: Fast, Top: true, Effort: true,
		price: Price{0.30, 2.50, 0.03, 0}, openRouter: "google/gemini-3.5-flash-lite"},
	{ID: "google:gemini-3.8-flash", Name: "gemini-3.8-flash", Title: "Gemini 3.8 Flash", Tier: Balanced, Top: true, Effort: true,
		price: Price{0.75, 3.75, 0.075, 0}, later: []Change{{newYear2027, Price{1.50, 7.50, 0.15, 0}}}, openRouter: "google/gemini-3.8-flash"},
}

// All is every model in the catalogue, including OpenRouter's copies of
// the others.
var All = func() []Model {
	var all []Model
	for _, m := range listed {
		m.Provider, m.Priced = providerOf(m.ID), true
		all = append(all, m)
	}
	for _, m := range all[:len(listed)] {
		if m.openRouter == "" {
			continue
		}
		or := m
		or.ID, or.Provider, or.Name, or.Title = string(OpenRouter)+":"+m.openRouter, OpenRouter, m.openRouter, m.Title+" (OpenRouter)"
		all = append(all, or)
	}
	return all
}()

func providerOf(id string) Provider {
	if p, _, ok := strings.Cut(id, ":"); ok {
		return Provider(p)
	}
	return Anthropic
}

var openRouterName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._:-]*$`)

// Find is the model named id: "claude-sonnet-5-5", "anthropic:claude-...",
// "openai:gpt-6-sol", or any "openrouter:vendor/model".
func Find(id string) (Model, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimPrefix(id, string(Anthropic)+":")
	for _, m := range All {
		if m.ID == id {
			return m, true
		}
	}
	if name, ok := strings.CutPrefix(id, string(OpenRouter)+":"); ok && openRouterName.MatchString(name) {
		return Model{ID: id, Provider: OpenRouter, Name: name, Title: name + " (OpenRouter)"}, true
	}
	return Model{}, false
}

// ForTier is the usual model for tier from the providers a company has, in
// order of preference. A provider without one for the tier offers its next
// best below it.
func ForTier(tier Tier, have []Provider) (Model, bool) {
	for _, p := range Providers {
		if !hasProvider(have, p) {
			continue
		}
		for t := tierIndex(tier); t >= 0; t-- {
			for _, m := range All {
				if m.Provider == p && m.Top && m.Tier == Tiers[t] {
					return m, true
				}
			}
		}
	}
	return Model{}, false
}

// AtMost says whether t is u or a cheaper tier.
func (t Tier) AtMost(u Tier) bool { return tierIndex(t) <= tierIndex(u) }

func tierIndex(t Tier) int {
	for i, x := range Tiers {
		if x == t {
			return i
		}
	}
	return len(Tiers) - 1
}

func hasProvider(have []Provider, p Provider) bool {
	for _, h := range have {
		if h == p {
			return true
		}
	}
	return false
}

var (
	// ErrUnknown means no model (or tier) has that name.
	ErrUnknown = errors.New("not a model GuildNet knows")
	// ErrNoKey means the company hasn't given the model's provider a key.
	ErrNoKey = errors.New("no key for its provider")
)

// Resolve is the model name means for a company with the providers have:
// a model id, or a tier ("fast", "balanced", "best"), which picks from its
// providers. A model whose provider it has no key for is refused.
func Resolve(name string, have []Provider) (Model, error) {
	for _, t := range Tiers {
		if strings.EqualFold(strings.TrimSpace(name), string(t)) {
			if m, ok := ForTier(t, have); ok {
				return m, nil
			}
			return Model{}, fmt.Errorf("%w: add a model provider's key to pick a %s model", ErrNoKey, t)
		}
	}
	m, ok := Find(name)
	if !ok {
		return Model{}, fmt.Errorf("%w: %q", ErrUnknown, strings.TrimSpace(name))
	}
	if !hasProvider(have, m.Provider) {
		return Model{}, fmt.Errorf("%w: %s is %s's, and there's no %s key", ErrNoKey, m.Title, m.Provider.Title(), m.Provider.Title())
	}
	return m, nil
}

// BaseURL is where p's OpenAI-compatible API is; "" for Anthropic, reached
// through Claude's own API.
func (p Provider) BaseURL() string {
	switch p {
	case OpenAI:
		return "https://api.openai.com/v1/"
	case Google:
		return "https://generativelanguage.googleapis.com/v1beta/openai/"
	case OpenRouter:
		return "https://openrouter.ai/api/v1/"
	}
	return ""
}

// KeyVar is the environment variable the runner finds p's key in.
func (p Provider) KeyVar() string {
	if p == Google {
		return "GEMINI_API_KEY"
	}
	return strings.ToUpper(string(p)) + "_API_KEY"
}

// Offered is what can be picked when hiring, from the providers a company
// has: current models, cheapest tier first.
func Offered(have []Provider) []Model {
	var out []Model
	for _, t := range Tiers {
		for _, m := range All {
			if m.Tier == t && !m.Older && hasProvider(have, m.Provider) {
				out = append(out, m)
			}
		}
	}
	return out
}
