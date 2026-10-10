// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"encoding/json"
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

// desksAtWork are desks working now, by lease ID.
type desksAtWork struct {
	mu     sync.Mutex
	leases map[string]deskLease
}

type deskLease struct {
	network, agent string
	started        time.Time
}

// brokerDeskStart is a network's runner asking whether one of its desks may
// work: yes (with the lease to report its time against), not now (as many
// desks as the policy allows are working), or no (the policy refuses). An
// agent carrying on after an approval isn't refused: its work was under
// way.
func (p *Platform) brokerDeskStart(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(r)
	if !ok {
		brokerError(w, http.StatusUnauthorized, "the network's secret is required")
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
	if !in.Resume && limits.Refused != "" {
		writeJSONResponse(w, http.StatusPaymentRequired, map[string]any{"error": "out of desk hours", "reason": limits.Refused})
		return
	}
	now := p.opts.Now()
	d := &p.desks
	d.mu.Lock()
	defer d.mu.Unlock()
	busy := 0
	for id, l := range d.leases {
		switch {
		case now.Sub(l.started) > deskKept:
			delete(d.leases, id)
		case l.network == n.Name && now.Sub(l.started) < deskHeld:
			busy++
		}
	}
	if limits.AtOnce > 0 && busy >= limits.AtOnce {
		writeJSONResponse(w, http.StatusTooManyRequests, map[string]any{"error": "the plan's desks are all working", "at_once": limits.AtOnce})
		return
	}
	if d.leases == nil {
		d.leases = map[string]deskLease{}
	}
	id := newToken()
	d.leases[id] = deskLease{network: n.Name, agent: in.Agent, started: now}
	writeJSONResponse(w, http.StatusOK, map[string]any{"id": id, "plan": limits.Plan})
}

// brokerDeskDone is a network's runner saying how long one of its desks
// worked: counted against the plan's hours, then credits, and never for
// more than has passed since it started.
func (p *Platform) brokerDeskDone(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(r)
	if !ok {
		brokerError(w, http.StatusUnauthorized, "the network's secret is required")
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
	d := &p.desks
	d.mu.Lock()
	l, ok := d.leases[in.ID]
	if ok && l.network == n.Name {
		delete(d.leases, in.ID)
	}
	d.mu.Unlock()
	if !ok || l.network != n.Name {
		brokerError(w, http.StatusNotFound, "no such desk at work")
		return
	}
	now := p.opts.Now()
	// By the second, part seconds rounded up, as the runner counts.
	seconds := min(in.Seconds, int64(math.Ceil(now.Sub(l.started).Seconds())))
	if err := p.hooks.Desks.Worked(r.Context(), n, l.agent, seconds, now); err != nil {
		monitor.Problem(p.opts.Logger, "counting %s's desk time on %s: %v", l.agent, n.Name, err)
		brokerError(w, http.StatusInternalServerError, "couldn't count the desk's time")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{})
}

// desksWorking is how many of network's desks are working now.
func (p *Platform) desksWorking(network string) int {
	now := p.opts.Now()
	p.desks.mu.Lock()
	defer p.desks.mu.Unlock()
	busy := 0
	for _, l := range p.desks.leases {
		if l.network == network && now.Sub(l.started) < deskHeld {
			busy++
		}
	}
	return busy
}
