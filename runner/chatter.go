// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Chatter: how much of an agents' work a channel sees. Steps go to the
// agent's away message as they happen either way.
const (
	chatterQuiet   = "quiet"   // only what it achieved
	chatterSummary = "summary" // that it's working, what it achieved, and a closing summary
	chatterSteps   = "steps"   // every step (a few a second at most), and what it achieved
)

// chatterFor is how much of nick's work shows where it's working: the
// setting of the team whose channel it is (in a DM, or a channel no team
// has, the agent's own team), or of the nearest team that team is inside
// with one; a summary if none has.
func (r *Runner) chatterFor(nick, channel string) string {
	ctx := context.Background()
	team := ""
	if channel != "" {
		teams, _ := r.store.Teams(ctx)
		for _, t := range teams {
			if strings.EqualFold(t.Channel, channel) {
				team = t.Name
			}
		}
	}
	if team == "" {
		if p, ok := r.persona(nick); ok {
			team = p.Team
		}
	}
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[strings.ToLower(cur)]; {
		seen[strings.ToLower(cur)] = true
		t, err := r.store.Team(ctx, cur)
		if err != nil {
			break
		}
		if t.Chatter != "" {
			return t.Chatter
		}
		cur = t.Parent
	}
	return chatterSummary
}

// maxWorkSteps is the most steps of one wake-up kept for the agent's page.
const maxWorkSteps = 200

// keepWork records a wake-up for the agent's page: where (a DM says only
// that), who asked and what (in a channel), its steps, how it ended, and
// what it cost.
func (r *Runner) keepWork(p config.Persona, a *activity, channel, asker, asked, outcome string, cost float64) {
	w := org.Work{Agent: p.Nick, Team: p.Team, Where: channel, Asker: asker, Asked: truncate(asked, 300),
		Started: a.started.UTC(), Finished: time.Now().UTC(), Outcome: truncate(outcome, 300), Cost: cost}
	if channel == "" {
		w.Where, w.Asker, w.Asked = "a DM", "", ""
	}
	a.mu.Lock()
	w.Steps = append([]org.WorkStep(nil), a.log...)
	a.mu.Unlock()
	if err := r.store.AddWork(context.Background(), w); err != nil {
		monitor.Problem(r.logger, "keeping %s's work: %v", p.Nick, err)
	}
}

// mayWatch says whether actor may see an agent's work: someone in its team,
// its manager, or whoever may see the team's log.
func (r *Runner) mayWatch(actor string, p config.Persona) bool {
	if strings.EqualFold(p.ReportsTo, actor) || r.can(actor, canLog, inTeam(p.Team)) {
		return true
	}
	m, err := r.store.Member(context.Background(), actor)
	return err == nil && p.Team != "" && strings.EqualFold(m.Team, p.Team)
}

func (r *Runner) apiWork(w http.ResponseWriter, req *http.Request) {
	actor := actorOf(req)
	p, ok := r.persona(req.PathValue("nick"))
	if !ok {
		apiError(w, http.StatusNotFound, "there's no agent "+req.PathValue("nick"))
		return
	}
	if !r.mayWatch(actor, p) {
		apiError(w, http.StatusForbidden, fmt.Sprintf("you can't see %s's work: it's for its team, its manager, and whoever may see the team's log", p.Nick))
		return
	}
	work, err := r.store.Work(req.Context(), p.Nick, 20)
	if err != nil {
		monitor.Problem(r.logger, "%s's work: %v", p.Nick, err)
		apiError(w, http.StatusInternalServerError, "couldn't read the work log")
		return
	}
	spend := r.can(actor, canSpend, inTeam(p.Team))
	type stepJSON struct {
		At      time.Time `json:"at"`
		Summary string    `json:"summary"`
		Result  bool      `json:"result,omitempty"`
	}
	type workJSON struct {
		Where    string     `json:"where"`
		Asker    string     `json:"asker,omitempty"`
		Asked    string     `json:"asked,omitempty"`
		Started  time.Time  `json:"started"`
		Finished time.Time  `json:"finished"`
		Steps    []stepJSON `json:"steps"`
		Outcome  string     `json:"outcome"`
		Cost     *float64   `json:"cost,omitempty"`
	}
	out := []workJSON{}
	for _, x := range work {
		v := workJSON{Where: x.Where, Asker: x.Asker, Asked: x.Asked, Started: x.Started, Finished: x.Finished, Outcome: x.Outcome, Steps: []stepJSON{}}
		for _, s := range x.Steps {
			v.Steps = append(v.Steps, stepJSON{s.At, s.Summary, s.Result})
		}
		if spend {
			cost := x.Cost
			v.Cost = &cost
		}
		out = append(out, v)
	}
	apiJSON(w, http.StatusOK, map[string]any{"work": out})
}

// outcomeOf is how a wake-up ended, in a few words.
func outcomeOf(res agent.Result, err error, approval int64) string {
	switch {
	case err != nil:
		return "failed: " + truncate(err.Error(), 280)
	case approval != 0:
		return fmt.Sprintf("waiting on approval #%d", approval)
	case res.Reply == "" || res.Reply == agent.Pass:
		return "passed"
	}
	return "answered"
}

// stepKind is what kind of step a step is, for the summary: its verb
// ("reads"), or the service it used ("uses Linear").
func stepKind(summary string) string {
	if rest, ok := strings.CutPrefix(summary, "uses "); ok {
		if service, _, found := strings.Cut(rest, ":"); found {
			return "uses " + service
		}
	}
	verb, _, _ := strings.Cut(summary, " ")
	return verb
}

// describeSteps is the steps taken, by kind, most first: "4 steps (reads
// 2, runs 1, uses Linear 1)".
func describeSteps(n int, kinds map[string]int, order []string) string {
	s := "1 step"
	if n != 1 {
		s = fmt.Sprintf("%d steps", n)
	}
	sorted := append([]string(nil), order...)
	sort.SliceStable(sorted, func(i, j int) bool { return kinds[sorted[i]] > kinds[sorted[j]] })
	var parts []string
	for i, k := range sorted {
		if i == 4 {
			parts = append(parts, fmt.Sprintf("and %d more kinds", len(sorted)-4))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", k, kinds[k]))
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

// took is how long something took, in a few characters: 38s, 4m 12s, 1h 5m.
func took(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
