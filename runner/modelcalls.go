// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"net/http"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
)

// Model calls: in GuildNet's hosted edition, agents that run outside
// GuildNet call models through it (its model gateway, package gateway),
// under /models/ on the runner's API, through the platform. Without one, a
// runner answers no model calls, and outside agents use keys of their own.

// UseModels has the handler h makes answer outside agents' model calls; h
// is given the network's runner to act on.
func (r *Runner) UseModels(h func(c *Core) http.Handler) {
	calls := h(&Core{r: r})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = calls
}

func (r *Runner) modelCalls() http.Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.models
}

// viaGateway is how the hosted edition's model gateway books its calls in
// the ledger, which the network's stats count apart.
const viaGateway = "gateway"

// Core is what an extension may use of a network's runner.
type Core struct{ r *Runner }

// Caller is the outside agent calling, or the status and why not: only
// through the platform (with the network's secret), and with the token of
// an agent that runs outside GuildNet now.
func (c *Core) Caller(req *http.Request) (nick string, status int, why string) {
	return c.r.outsideCaller(req)
}

// Agent is one of the network's agents.
func (c *Core) Agent(ctx context.Context, nick string) (config.Persona, error) {
	return c.r.store.Agent(ctx, nick)
}

// OwnModel is the model an agent was given, or the network's usual one.
func (c *Core) OwnModel(p config.Persona) (models.Model, bool) {
	if m, ok := models.Find(p.Model); ok {
		return m, true
	}
	return models.Find(c.r.defaultModel())
}

// Paused says whether the network's agents are paused.
func (c *Core) Paused() bool { return c.r.isPaused() }

// Ledger is the network's spend.
func (c *Core) Ledger() *ledger.Ledger { return c.r.ledger }

// Blocked says, where people see it, that nick's work was turned away for
// why (out of budget).
func (c *Core) Blocked(nick, why string) { c.r.noticeBlocked(nick, why) }

// Spent says, where people see it, which shares of the budgets spending
// just crossed (from the ledger's Record).
func (c *Core) Spent(crossed []int) { c.r.announceBudget(crossed) }
