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

const (
	// deskHeld is how long a desk that started holds its place among the
	// plan's desks at once, if its runner never says it finished.
	deskHeld = 2 * time.Hour
	// deskKept is how long a desk's time can still be reported.
	deskKept = 24 * time.Hour
)

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
	if err := leases.ForgetDeskLeases(r.Context(), now.Add(-deskKept)); err != nil {
		monitor.Problem(p.opts.Logger, "forgetting old desk leases: %v", err)
	}
	busy, err := leases.DesksAtWork(r.Context(), n.Name, now.Add(-deskHeld))
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
	busy, err := p.leases().DesksAtWork(context.Background(), network, p.opts.Now().Add(-deskHeld))
	if err != nil {
		monitor.Problem(p.opts.Logger, "desks at work on %s: %v", network, err)
	}
	return busy
}
