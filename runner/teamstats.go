// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/org"
)

// A team's numbers, counting the teams inside it: who's in it, approvals
// waiting on its agents and how long the last month's took to decide, when
// each of its agents last said anything, and what it spent this month (for
// those who may see spend there). Numbers only: anyone in the org may look.

func (r *Runner) apiTeamStats(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	team, err := r.store.Team(ctx, req.PathValue("name"))
	if err != nil {
		apiRefusal(w, r.storeRefusal(req.PathValue("name"), err))
		return
	}
	in := func(t string) bool { return r.within(t, []string{team.Name}) }
	out := map[string]any{"team": team.Name}

	humans, agents := 0, map[string]bool{}
	members, _ := r.store.Members(ctx)
	for _, m := range members {
		if in(m.Team) {
			humans++
		}
	}
	for _, p := range r.staff() {
		if in(p.Team) {
			agents[strings.ToLower(p.Nick)] = true
		}
	}
	out["humans"], out["agents"] = humans, len(agents)

	now := r.now()
	recent, _ := r.store.ApprovalsSince(ctx, now.AddDate(0, 0, -30))
	waiting := 0
	var took []float64
	for _, a := range recent {
		if !agents[strings.ToLower(a.Agent)] {
			continue
		}
		switch a.Status {
		case org.Pending:
			waiting++
		case org.Approved, org.Denied:
			took = append(took, a.DecidedAt.Sub(a.RaisedAt).Minutes())
		}
	}
	out["waiting"], out["decided"] = waiting, len(took)
	if len(took) > 0 {
		slices.Sort(took)
		out["median_minutes"] = took[len(took)/2]
	}

	lastSaid := map[string]time.Time{}
	for _, ch := range r.allChannels() {
		for _, l := range r.chat.Recent(strings.ToLower(ch), archiveLines) {
			k := strings.ToLower(l.Nick)
			if agents[k] && l.At.After(lastSaid[k]) {
				lastSaid[k] = l.At
			}
		}
	}
	named := map[string]time.Time{}
	for _, p := range r.staff() {
		if t, ok := lastSaid[strings.ToLower(p.Nick)]; ok {
			named[p.Nick] = t
		}
	}
	out["last_said"] = named

	if r.can(actorOf(req), canSpend, inTeam(team.Name)) {
		spent := 0.0
		for _, d := range r.ledger.ThisMonth() {
			for a, v := range d.ByAgent {
				if agents[strings.ToLower(a)] {
					spent += v
				}
			}
		}
		out["spend_month"] = spent
	}
	apiJSON(w, http.StatusOK, out)
}
