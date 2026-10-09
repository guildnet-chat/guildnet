// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"

	"github.com/guildnet-chat/guildnet/ledger"
)

// Meter keeps a wake-up within its budgets: it's asked before each model
// call, and told what each one cost as it comes back, so a wake-up of many
// calls stops when the money runs out rather than after.
type Meter interface {
	// Allow says why another call mustn't start, if it mustn't.
	Allow() error
	// Charge books a call: its usage on model, or cost when the platform
	// says what it charged (0 means work it out from the usage).
	Charge(model string, u ledger.Usage, cost float64)
}

type meterKey struct{}

// WithMeter is ctx with m keeping what's done under it within budget.
func WithMeter(ctx context.Context, m Meter) context.Context {
	return context.WithValue(ctx, meterKey{}, m)
}

// MeterOf is ctx's meter, or nil when nothing's counting.
func MeterOf(ctx context.Context) Meter {
	m, _ := ctx.Value(meterKey{}).(Meter)
	return m
}
