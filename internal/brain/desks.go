// SPDX-License-Identifier: AGPL-3.0-only

package brain

import (
	"context"
	"fmt"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// deskRounds caps the model calls in one wake-up at a desk: desk work takes
// more steps than a reply in chat.
const deskRounds = 50

// DeskOptions configure desks of GuildNet's own.
type DeskOptions struct {
	// Toolbox is what an agent at a desk has: its workspace's tools and
	// GuildNet's.
	Toolbox agent.Toolbox
	// Ready readies p's desk before it works (wakes its sandbox, checks out
	// its repos). The agent doesn't start if it fails.
	Ready func(ctx context.Context, p config.Persona) error
	// Retire clears p's desk when it's fired; nil for nothing to clear.
	Retire func(ctx context.Context, p config.Persona) error
}

// Desks are agents at desks of GuildNet's own, on any model: the runner's
// own loop, with a workspace. It implements the runner's AgentPlatform and
// Staffing.
type Desks struct {
	b    *Brain
	opts DeskOptions
}

// Desks is the agent platform for desks of GuildNet's own, on this brain's
// models and keys.
func (b *Brain) Desks(o DeskOptions) *Desks {
	return &Desks{b: b, opts: o}
}

func (d *Desks) loop() loop {
	return loop{b: d.b, toolbox: d.opts.Toolbox, kit: agent.Desk, rounds: deskRounds}
}

func (d *Desks) ready(ctx context.Context, p config.Persona) error {
	if d.opts.Ready == nil {
		return nil
	}
	if err := d.opts.Ready(ctx, p); err != nil {
		return fmt.Errorf("%s's desk isn't ready: %w", p.Nick, err)
	}
	return nil
}

// Respond wakes the agent at its desk.
func (d *Desks) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	if err := d.ready(ctx, req.Persona); err != nil {
		return agent.Result{}, err
	}
	return d.loop().start(ctx, req, onStep)
}

// Resume answers the approval the agent was waiting on and carries on, at a
// desk readied again: its sandbox may have slept while a person decided.
func (d *Desks) Resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error) {
	if _, err := parseResume(token); err != nil {
		return agent.Result{}, err
	}
	if err := d.ready(ctx, p); err != nil {
		return agent.Result{}, err
	}
	return d.loop().resume(ctx, p, token, allow, reason, onStep)
}

// Hire needs nothing: a desk is made when its agent first works.
func (d *Desks) Hire(context.Context, config.Persona) error { return nil }

// Update needs nothing: each wake-up reads the agent afresh.
func (d *Desks) Update(context.Context, config.Persona) error { return nil }

// Fire clears the agent's desk.
func (d *Desks) Fire(ctx context.Context, p config.Persona) error {
	if d.opts.Retire == nil {
		return nil
	}
	return d.opts.Retire(ctx, p)
}
