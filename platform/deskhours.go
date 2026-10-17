// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Desks at work: a network's runner asks the broker before one of its desks
// works, and says after how long it worked. The broker keeps the desks at
// work and checks the runner; the extension's desk policy (Hooks.Desks)
// decides whether a desk may work and counts its time. Without one, /desks/*
// isn't there, and runners take that to mean desk time isn't counted.

// The hosted edition's plans, desk hours and credits (cloud) are kept in the
// platform's database until it keeps its own (OSS 08).

// The memory store's.

// Postgres's.

// Desks at work.

// deskLost is how long a desk's runner can go without saying it's still
// working (it does every minute) before the desk is taken for lost: it lets
// go of its place among the plan's desks at once, and is charged up to the
// last time it said so.
const deskLost = 5 * time.Minute

// desksAtWork are desks working now, by lease ID, for a store that keeps
// none (DeskLeases); mu also makes each desk's start one at a time, so two
// can't both take a network's last place.
type desksAtWork struct {
	mu     sync.Mutex
	leases leaseMap
	start  sync.Mutex
}

// brokerDeskStart is a network's runner asking whether one of its desks may
// work: yes (with the lease to report its time against), not now (as many
// desks as the policy allows are working), or no (the policy refuses). An
// agent carrying on after an approval is refused only if the policy says
// so of that too (DeskLimits.ResumeRefused): its work was under way.
func (p *Platform) brokerDeskStart(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	var in struct {
		Agent  string `json:"agent"`
		Resume bool   `json:"resume"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in); err != nil || in.Agent == "" {
		brokerError(w, http.StatusBadRequest, "say which agent's desk")
		return
	}
	limits, err := p.hooks.Desks.Limits(r.Context(), n, in.Agent)
	if err != nil {
		monitor.Problem(p.opts.Logger, "desk hours for %s: %v", n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't check the network's desk hours")
		return
	}
	why := limits.Refused
	if in.Resume {
		why = limits.ResumeRefused
	}
	if why != "" {
		writeJSONResponse(w, http.StatusPaymentRequired, map[string]any{"error": "out of desk hours", "reason": why})
		return
	}
	now := p.opts.Now()
	leases := p.leases()
	p.desks.start.Lock()
	defer p.desks.start.Unlock()
	p.chargeLostDesks(r.Context(), now)
	busy, err := leases.DesksAtWork(r.Context(), n.Name, now.Add(-deskLost))
	if err != nil {
		monitor.Problem(p.opts.Logger, "desks at work on %s: %v", n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't check the network's desks at work")
		return
	}
	if limits.AtOnce > 0 && busy >= limits.AtOnce {
		writeJSONResponse(w, http.StatusTooManyRequests, map[string]any{"error": "the plan's desks are all working", "at_once": limits.AtOnce})
		return
	}
	id := newToken()
	if err := leases.StartDeskLease(r.Context(), id, DeskLease{Network: n.Name, Agent: in.Agent, Started: now}); err != nil {
		monitor.Problem(p.opts.Logger, "keeping %s's desk lease on %s: %v", in.Agent, n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't keep the desk's place")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"id": id, "plan": limits.Plan})
}

// chargeLostDesks lets go of desks whose runners have stopped saying
// they're working (one restarted mid-desk, say), and charges each up to the
// last time its runner said so, then.
func (p *Platform) chargeLostDesks(ctx context.Context, now time.Time) {
	lost, err := p.leases().TakeLostDeskLeases(ctx, now.Add(-deskLost))
	if err != nil {
		monitor.Problem(p.opts.Logger, "finding lost desks: %v", err)
		return
	}
	for _, l := range lost {
		seconds := int64(math.Ceil(l.Beat.Sub(l.Started).Seconds()))
		if seconds <= 0 {
			continue
		}
		n, err := p.opts.Networks.Network(ctx, l.Network)
		if err == nil {
			err = p.hooks.Desks.Worked(ctx, n, l.Agent, seconds, l.Beat)
		}
		if err != nil {
			monitor.Problem(p.opts.Logger, "counting %s's lost desk's %ds on %s: %v", l.Agent, seconds, l.Network, err)
		}
	}
}

// brokerDeskBeat is a network's runner saying one of its desks is still
// working.
func (p *Platform) brokerDeskBeat(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in); err != nil || in.ID == "" {
		brokerError(w, http.StatusBadRequest, "say which desk")
		return
	}
	err := p.leases().BeatDeskLease(r.Context(), in.ID, n.Name, p.opts.Now())
	switch {
	case errors.Is(err, ErrNotFound):
		brokerError(w, http.StatusNotFound, "no such desk at work: it was taken for lost")
	case err != nil:
		monitor.Problem(p.opts.Logger, "a desk's word on %s: %v", n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't take the desk's word")
	default:
		writeJSONResponse(w, http.StatusOK, map[string]any{})
	}
}

// brokerDeskDone is a network's runner saying how long one of its desks
// worked: counted against the plan's hours, then credits, and never for
// more than has passed since it started.
func (p *Platform) brokerDeskDone(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	var in struct {
		ID      string `json:"id"`
		Seconds int64  `json:"seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in); err != nil || in.Seconds < 0 {
		brokerError(w, http.StatusBadRequest, "say which desk, and how many seconds it worked")
		return
	}
	l, err := p.leases().EndDeskLease(r.Context(), in.ID, n.Name)
	if errors.Is(err, ErrNotFound) {
		brokerError(w, http.StatusNotFound, "no such desk at work")
		return
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "ending a desk lease on %s: %v", n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't find the desk at work")
		return
	}
	now := p.opts.Now()
	// By the second, part seconds rounded up, as the runner counts.
	seconds := min(in.Seconds, int64(math.Ceil(now.Sub(l.Started).Seconds())))
	if err := p.hooks.Desks.Worked(r.Context(), n, l.Agent, seconds, now); err != nil {
		monitor.Problem(p.opts.Logger, "counting %s's desk time on %s: %v", l.Agent, n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't count the desk's time")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{})
}

// desksWorking is how many of network's desks are working now.
func (p *Platform) desksWorking(network string) int {
	busy, err := p.leases().DesksAtWork(context.Background(), network, p.opts.Now().Add(-deskLost))
	if err != nil {
		monitor.Problem(p.opts.Logger, "desks at work on %s: %v", network, err)
	}
	return busy
}
