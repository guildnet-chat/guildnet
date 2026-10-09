// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
)

// AgentPlatform is where an agent's wake-ups run: a plain model call, a desk
// of GuildNet's own, or a desk on Managed Agents.
type AgentPlatform interface {
	Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error)
	Resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error)
}

const (
	// PlatformManaged is the platform value for agents with a desk on
	// Managed Agents (Claude only).
	PlatformManaged = "managed"
	// PlatformDesk is the platform value for agents with a desk of
	// GuildNet's own: a workspace in GuildNet's sandbox, on any model.
	PlatformDesk = "desk"
)

// Router runs each agent on its own platform (config.Persona.Platform).
// Relevance checks and catch-ups always use the chat brain: they're small
// model calls that need no tools.
type Router struct {
	chat    Brain
	managed AgentPlatform // nil when Managed Agents isn't set up
	desks   AgentPlatform // nil when GuildNet's sandbox isn't set up
	meter   DeskMetering  // counts desks' time; its Meter is nil when nothing does
}

// DeskMeter says whether a desk of GuildNet's own may work, and counts the
// time it does: GuildNet's platform, through its broker (desk.Broker).
type DeskMeter interface {
	StartDesk(ctx context.Context, agent string, resume bool) (desk.Turn, error)
	DeskDone(ctx context.Context, id string, seconds int64) error
}

// DeskMetering is how desks' time is counted.
type DeskMetering struct {
	Meter  DeskMeter
	Logger *log.Logger
	// Retry is how often to ask again while every desk is busy (15s), and
	// Wait how long a wake-up waits for one before the agent works in chat
	// (10 minutes). An agent carrying on after an approval waits as long as
	// its wake-up may take.
	Retry, Wait time.Duration
}

// MeterDesks counts desks' time with m.
func (r *Router) MeterDesks(m DeskMetering) *Router {
	if m.Retry <= 0 {
		m.Retry = 15 * time.Second
	}
	if m.Wait <= 0 {
		m.Wait = 10 * time.Minute
	}
	r.meter = m
	return r
}

// NewRouter routes agents to chat or to managed. managed may be nil.
func NewRouter(chat Brain, managed AgentPlatform) *Router {
	return &Router{chat: chat, managed: managed}
}

// WithDesks routes agents on the desk platform to desks.
func (r *Router) WithDesks(desks AgentPlatform) *Router {
	r.desks = desks
	return r
}

// at is the platform p is on, if this network has it.
func (r *Router) at(p config.Persona) AgentPlatform {
	switch p.Platform {
	case PlatformManaged:
		return r.managed
	case PlatformDesk:
		return r.desks
	}
	return nil
}

func (r *Router) pick(p config.Persona) AgentPlatform {
	if a := r.at(p); a != nil {
		return a
	}
	return r.chat
}

// HasPlatform reports whether agents can be moved to platform.
func (r *Router) HasPlatform(platform string) bool {
	switch platform {
	case "", "chat":
		return true
	case PlatformManaged:
		return r.managed != nil
	case PlatformDesk:
		return r.desks != nil
	}
	return false
}

func (r *Router) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	a := r.pick(req.Persona)
	if !r.metered(req.Persona) {
		return a.Respond(ctx, req, onStep)
	}
	turn, why, err := r.deskTurn(ctx, req.Persona.Nick, false, onStep)
	if err != nil {
		return agent.Result{}, err
	}
	if why != "" {
		// The runner's own word, always shown, and not counted as a step.
		onStep(agent.Step{Summary: "is working in chat for now, without its desk: " + why + ".", Result: true})
		return r.chat.Respond(ctx, req, onStep)
	}
	ctx, waited := r.waitingForRoom(ctx, onStep)
	defer r.deskDone(ctx, req.Persona.Nick, turn, time.Now(), waited)
	return a.Respond(ctx, req, onStep)
}

// waitingForRoom is ctx counting how long the desk waits for room at the
// sandbox (left out of its desk time), saying so when it starts waiting.
func (r *Router) waitingForRoom(ctx context.Context, onStep func(agent.Step)) (context.Context, func() time.Duration) {
	return desk.CountWaiting(ctx, func() {
		onStep(agent.Step{Summary: "is waiting for room at the sandbox: every desk there is working", Result: true})
	})
}

func (r *Router) Resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error) {
	a := r.pick(p)
	if !r.metered(p) {
		return a.Resume(ctx, p, token, allow, reason, onStep)
	}
	turn, why, err := r.deskTurn(ctx, p.Nick, true, onStep)
	if err != nil {
		return agent.Result{}, err
	}
	if why != "" {
		return agent.Result{}, fmt.Errorf("%s's desk can't carry on: %s", p.Nick, why)
	}
	ctx, waited := r.waitingForRoom(ctx, onStep)
	defer r.deskDone(ctx, p.Nick, turn, time.Now(), waited)
	return a.Resume(ctx, p, token, allow, reason, onStep)
}

// metered says whether p's wake-ups are counted: at a desk of GuildNet's
// own, where desks' time is counted. Managed Agents' desks run on the
// company's own Anthropic account.
func (r *Router) metered(p config.Persona) bool {
	return p.Platform == PlatformDesk && r.desks != nil && r.meter.Meter != nil
}

// deskTurn waits for nick's desk to be given a turn: why it can't have one
// (so the agent works in chat), or an error if the wake-up ran out of time
// waiting. If the platform can't be asked, the desk works uncounted.
func (r *Router) deskTurn(ctx context.Context, nick string, resume bool, onStep func(agent.Step)) (desk.Turn, string, error) {
	m := r.meter
	giveUp := time.Now().Add(m.Wait)
	said := false
	for {
		turn, err := m.Meter.StartDesk(ctx, nick, resume)
		switch {
		case err != nil:
			monitor.Problem(m.Logger, "desks: asking whether %s's desk may work: %v", nick, err)
			return desk.Turn{}, "", nil
		case turn.Refused != "":
			return desk.Turn{}, turn.Refused, nil
		case !turn.Busy:
			return turn, "", nil
		}
		if !said {
			onStep(agent.Step{Summary: "is waiting for a desk: all of this network's desks are working", Result: true})
			said = true
		}
		if !resume && time.Now().After(giveUp) {
			return desk.Turn{}, "all of this network's desks stayed busy", nil
		}
		select {
		case <-ctx.Done():
			return desk.Turn{}, "", fmt.Errorf("%s waited for a desk until it ran out of time: %w", nick, ctx.Err())
		case <-time.After(m.Retry):
		}
	}
}

// deskDone says how long a desk worked since start, less what it waited
// for room at the sandbox, by the second (part seconds rounded up), even if
// the wake-up itself ran out of time.
func (r *Router) deskDone(ctx context.Context, nick string, turn desk.Turn, start time.Time, waited func() time.Duration) {
	if turn.ID == "" {
		return
	}
	seconds := int64(math.Ceil(max(time.Since(start)-waited(), 0).Seconds()))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := r.meter.Meter.DeskDone(ctx, turn.ID, seconds); err != nil {
		monitor.Problem(r.meter.Logger, "desks: reporting %s's %ds at its desk: %v", nick, seconds, err)
	}
}

func (r *Router) PickResponders(ctx context.Context, model, channel, topic string, transcript []chat.Line, candidates []config.Persona) ([]string, ledger.Usage, error) {
	return r.chat.PickResponders(ctx, model, channel, topic, transcript, candidates)
}

func (r *Router) Catchup(ctx context.Context, req agent.CatchupRequest) (string, ledger.Usage, error) {
	return r.chat.Catchup(ctx, req)
}

// staffing is the platform that keeps its own copy of p, if any.
func (r *Router) staffing(p config.Persona) (Staffing, bool) {
	s, ok := r.at(p).(Staffing)
	return s, ok
}

func (r *Router) Hire(ctx context.Context, p config.Persona) error {
	if s, ok := r.staffing(p); ok {
		return s.Hire(ctx, p)
	}
	return nil
}

func (r *Router) Update(ctx context.Context, p config.Persona) error {
	if s, ok := r.staffing(p); ok {
		return s.Update(ctx, p)
	}
	return nil
}

func (r *Router) Fire(ctx context.Context, p config.Persona) error {
	if s, ok := r.staffing(p); ok {
		return s.Fire(ctx, p)
	}
	return nil
}

// deskProblem says why p can't be at a desk on its model, if it can't:
// desks are on Managed Agents, which runs Claude.
func deskProblem(p config.Persona) string {
	if p.Platform != PlatformManaged {
		return ""
	}
	if m, ok := models.Find(p.Model); ok && m.Provider != models.Anthropic {
		return fmt.Sprintf("desks run Claude (they're on Anthropic's Managed Agents), and %s is on %s. Move %s to a Claude model first, or keep it in chat.", p.Nick, m.Title, p.Nick)
	}
	return ""
}

// atDesk reports whether p works at a desk, of either kind.
func atDesk(p config.Persona) bool {
	return p.Platform == PlatformManaged || p.Platform == PlatformDesk
}

// timeoutFor is how long one wake-up of p may take: an agent at a desk may
// work through many tool calls; a plain reply is quick.
func (r *Runner) timeoutFor(p config.Persona) time.Duration {
	if atDesk(p) {
		if r.cfg.DeskTimeout > 0 {
			return r.cfg.DeskTimeout
		}
		return deskTimeout
	}
	if r.cfg.ReplyTimeout > 0 {
		return r.cfg.ReplyTimeout
	}
	return replyTimeout
}
