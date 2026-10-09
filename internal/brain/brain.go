// SPDX-License-Identifier: AGPL-3.0-only

// Package brain calls each agent's model, at its provider: a reply, picking
// who answers, a catch-up. It's how the runner reaches models; what agents
// are told is package agent's. Only the runner imports it, so the model
// providers' SDKs are built into nothing else.
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
)

// Brain calls each persona's model, at its provider, on its behalf.
type Brain struct {
	mu       sync.RWMutex // guards callers, which change with the keys
	callers  map[models.Provider]caller
	baseURLs map[models.Provider]string
	company  string
	owner    string
	toolbox  agent.Toolbox
}

// Options configure a Brain.
type Options struct {
	Company, Owner string
	// Toolbox is the tools agents in chat can use (GitHub's, for agents
	// without a workspace); nil for none.
	Toolbox agent.Toolbox
	// Keys are the network's model providers' keys.
	Keys map[models.Provider]string
	// BaseURLs override where providers are reached (tests).
	BaseURLs map[models.Provider]string
}

// New is a brain for the providers it has keys for.
func New(o Options) *Brain {
	return &Brain{callers: callersFor(o.Keys, o.BaseURLs), baseURLs: o.BaseURLs, company: strings.TrimSpace(o.Company), owner: o.Owner, toolbox: o.Toolbox}
}

// SetKeys replaces the providers' keys, from the next call on.
func (b *Brain) SetKeys(keys map[models.Provider]string) {
	callers := callersFor(keys, b.baseURLs)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.callers = callers
}

// Providers are the providers it has keys for, in order of preference.
func (b *Brain) Providers() []models.Provider {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []models.Provider
	for _, p := range models.Providers {
		if _, ok := b.callers[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Broker is the platform's broker, where a runner fetches its network's
// model keys, with the network's secret. The keys are kept only in memory.
type Broker struct {
	URL, Network, Secret string
	// HTTP is how the broker is reached (one trusting GuildNet's own CA, for
	// TLS inside the VM); nil means a plain client.
	HTTP *http.Client
}

// Keys fetches the network's model keys.
func (k Broker) Keys(ctx context.Context) (map[models.Provider]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(k.URL, "/")+"/model-keys", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("GuildNet-Network", k.Network)
	req.Header.Set("Authorization", "Bearer "+k.Secret)
	client := k.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking the broker for model keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the broker didn't give model keys: %s", resp.Status)
	}
	var out struct {
		Keys map[models.Provider]string `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return nil, errors.New("the broker's answer wasn't model keys")
	}
	return out.Keys, nil
}

// KeysFromEnv are the providers' keys in the environment
// (ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, OPENROUTER_API_KEY):
// a developer's machine, with no broker.
func KeysFromEnv() map[models.Provider]string {
	keys := map[models.Provider]string{}
	for _, p := range models.Providers {
		if k := os.Getenv(p.KeyVar()); k != "" {
			keys[p] = k
		}
	}
	return keys
}

// ProvidersOf are the providers keys has, in order of preference.
func ProvidersOf(keys map[models.Provider]string) []models.Provider {
	var out []models.Provider
	for _, p := range models.Providers {
		if keys[p] != "" {
			out = append(out, p)
		}
	}
	return out
}

// complete makes one call to model, at its provider, if the wake-up's meter
// (if it has one) allows it, and charges it there as it comes back.
func (b *Brain) complete(ctx context.Context, model string, x call) (answer, error) {
	m := agent.MeterOf(ctx)
	if m != nil {
		if err := m.Allow(); err != nil {
			return answer{}, err
		}
	}
	a, err := b.completeAt(ctx, model, x)
	if m != nil && (a.usage != (ledger.Usage{}) || a.cost > 0) {
		m.Charge(model, a.usage, a.cost)
	}
	return a, err
}

func (b *Brain) completeAt(ctx context.Context, model string, x call) (answer, error) {
	m, ok := models.Find(model)
	if !ok {
		// Not in the catalogue: Claude's API says whether it's a model.
		m = models.Model{ID: model, Provider: models.Anthropic, Name: model, Title: model}
	}
	b.mu.RLock()
	c, ok := b.callers[m.Provider]
	b.mu.RUnlock()
	if !ok {
		return answer{}, fmt.Errorf("%s is %s's, and this network has no %s key", m.Title, m.Provider.Title(), m.Provider.Title())
	}
	x.model = m
	if !m.Effort {
		x.effort = ""
	}
	return c.call(ctx, x)
}

// PickResponders asks a small model which agents, if any, should answer a
// human message that didn't address anyone by nick.
func (b *Brain) PickResponders(ctx context.Context, model, channel, topic string, transcript []chat.Line, candidates []config.Persona) ([]string, ledger.Usage, error) {
	// Room to think on reasoning models, kept short: the answer is a word or
	// two.
	a, err := b.complete(ctx, model, call{prompt: agent.PickPrompt(channel, topic, transcript, candidates), maxTokens: 1000, effort: "low"})
	if err != nil {
		return nil, a.usage, err
	}
	return agent.Picked(a.text, candidates), a.usage, nil
}

// Catchup writes a short IRC-ready summary of what req.For missed.
func (b *Brain) Catchup(ctx context.Context, req agent.CatchupRequest) (string, ledger.Usage, error) {
	a, err := b.complete(ctx, req.Model, call{prompt: agent.CatchupPrompt(req), maxTokens: 2000, effort: "low"})
	if err != nil {
		return "", a.usage, err
	}
	return a.text, a.usage, nil
}
