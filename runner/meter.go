// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"errors"
	"sync"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/ledger"
)

// errPaused is why a wake-up stops when the network's agents are paused
// while it works.
var errPaused = errors.New("all agents were paused")

// meter keeps one wake-up of p's within the budgets (agent.Meter): another
// model call may start only while p and the network have money left today,
// and each is booked as it comes back. Agents working at once can each
// still start one call past the budget, no more.
type meter struct {
	r     *Runner
	p     config.Persona
	where string

	mu      sync.Mutex
	charged float64
	usage   ledger.Usage
}

func (r *Runner) meter(p config.Persona, where string) *meter {
	return &meter{r: r, p: p, where: where}
}

func (m *meter) Allow() error {
	if m.r.isPaused() {
		return errPaused
	}
	if ok, why := m.r.ledger.Allowed(m.p.Nick, m.p.DailyBudgetUSD); !ok {
		return errors.New(why)
	}
	return nil
}

func (m *meter) Charge(model string, u ledger.Usage, cost float64) {
	var booked float64
	if cost > 0 {
		booked = m.r.recordResult(m.p.Nick, model, m.where, agent.Result{Usage: u, Cost: cost})
	} else {
		booked = m.r.record(m.p.Nick, model, m.where, u)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charged += booked
	m.usage = plus(m.usage, u)
}

// settle books what of res wasn't charged as it went (a platform that
// says what the whole wake-up cost, say), and says what it all cost.
func (m *meter) settle(res agent.Result) float64 {
	m.mu.Lock()
	charged, usage := m.charged, m.usage
	m.mu.Unlock()
	if res.Cost > 0 {
		if rest := res.Cost - charged; rest > 1e-9 {
			charged += m.r.recordResult(m.p.Nick, m.p.Model, m.where, agent.Result{Cost: rest})
		}
		return charged
	}
	rest := ledger.Usage{Input: res.Usage.Input - usage.Input, Output: res.Usage.Output - usage.Output,
		CacheWrite: res.Usage.CacheWrite - usage.CacheWrite, CacheRead: res.Usage.CacheRead - usage.CacheRead}
	if rest.Input > 0 || rest.Output > 0 || rest.CacheWrite > 0 || rest.CacheRead > 0 {
		charged += m.r.record(m.p.Nick, m.p.Model, m.where, ledger.Usage{Input: max(rest.Input, 0), Output: max(rest.Output, 0),
			CacheWrite: max(rest.CacheWrite, 0), CacheRead: max(rest.CacheRead, 0)})
	}
	return charged
}

func plus(a, b ledger.Usage) ledger.Usage {
	return ledger.Usage{Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheWrite: a.CacheWrite + b.CacheWrite, CacheRead: a.CacheRead + b.CacheRead}
}
