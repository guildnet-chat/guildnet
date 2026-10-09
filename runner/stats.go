// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Stats are how a network is used, for GuildNet's own admins: numbers only.
// Never who is in it, what anyone said, or what its agents did.

func (r *Runner) apiStats(w http.ResponseWriter, req *http.Request) {
	ctx := context.Background()
	people, guests := 0, 0
	ownerListed := false
	members, _ := r.store.Members(ctx)
	for _, m := range members {
		switch {
		case m.Guest:
			guests++
		default:
			people++
		}
		if r.isOwner(m.Account) {
			ownerListed = true
		}
	}
	if !ownerListed {
		people++
	}
	teams, _ := r.store.Teams(ctx)
	today, calls := r.ledger.Today()
	type dayJSON struct {
		Day   string  `json:"day"`
		Total float64 `json:"total"`
		Calls int     `json:"calls"`
	}
	days := []dayJSON{}
	for _, d := range r.ledger.ThisMonth() {
		days = append(days, dayJSON{Day: d.Day, Total: d.Total, Calls: d.Calls})
	}
	gatewaySpend, gatewayCalls := r.ledger.Via(viaGateway)
	out := map[string]any{
		"gateway_today":       gatewaySpend,
		"gateway_calls_today": gatewayCalls,
		"people":              people,
		"guests":              guests,
		"agents":              len(r.staff()),
		"teams":               len(teams),
		"spend_today":         today,
		"calls_today":         calls,
		"daily_budget":        r.ledger.Budget(),
		"spend_month":         r.ledger.Month(),
		"days":                days,
		"last_activity":       nil,
	}
	if last := r.lastActivity(); !last.IsZero() {
		out["last_activity"] = last.UTC().Format(time.RFC3339)
	}
	apiJSON(w, http.StatusOK, out)
}

// apiSpend is what the network's agents have spent, as the person asking
// may see it: everything for whoever may see spend across the org (the
// owner, and admins), with the network's daily cap; otherwise the agents
// they may see spend for (their teams' by default, for leads), with totals
// over just those and their caps added up. Someone who may see none is
// refused: it's the company's money.
func (r *Runner) apiSpend(w http.ResponseWriter, req *http.Request) {
	actor := actorOf(req)
	whole := r.can(actor, canSpend, wholeOrg)
	visible := map[string]bool{}
	for _, p := range r.staff() {
		if whole || r.can(actor, canSpend, onPerson(p.Nick)) {
			visible[strings.ToLower(p.Nick)] = true
		}
	}
	if !whole && !r.holdsAbility(actor, canSpend) {
		apiError(w, http.StatusForbidden, "you can't see spend here: ask the owner, or your team's lead.")
		return
	}
	type dayJSON struct {
		Day     string             `json:"day"`
		Total   float64            `json:"total"`
		Calls   int                `json:"calls"`
		ByAgent map[string]float64 `json:"by_agent"`
	}
	type agentJSON struct {
		Nick        string  `json:"nick"`
		Team        string  `json:"team"`
		Model       string  `json:"model"`
		DailyBudget float64 `json:"daily_budget"`
		Today       float64 `json:"today"`
		Month       float64 `json:"month"`
	}
	days := []dayJSON{}
	month, byAgentMonth := 0.0, map[string]float64{}
	for _, d := range r.ledger.ThisMonth() {
		day := dayJSON{Day: d.Day, Total: d.Total, Calls: d.Calls, ByAgent: map[string]float64{}}
		if !whole {
			day.Total, day.Calls = 0, 0 // calls aren't kept by agent, so only the whole org's count
		}
		for a, v := range d.ByAgent {
			if whole || visible[strings.ToLower(a)] {
				day.ByAgent[a] = v
				if !whole {
					day.Total += v
				}
			}
		}
		days = append(days, day)
		month += day.Total
		for a, v := range day.ByAgent {
			byAgentMonth[strings.ToLower(a)] += v
		}
	}
	today := days[len(days)-1]
	agents := []agentJSON{}
	caps := 0.0
	for _, p := range r.staff() {
		k := strings.ToLower(p.Nick)
		if !visible[k] {
			continue
		}
		a := agentJSON{Nick: p.Nick, Team: p.Team, Model: p.Model, DailyBudget: p.DailyBudgetUSD, Month: byAgentMonth[k]}
		for n, v := range today.ByAgent {
			if strings.EqualFold(n, p.Nick) {
				a.Today += v
			}
		}
		caps += p.DailyBudgetUSD
		agents = append(agents, a)
	}
	scope, budget := "org", r.ledger.Budget()
	if !whole {
		scope, budget = "part", caps
	}
	apiJSON(w, http.StatusOK, map[string]any{
		"scope":        scope,
		"daily_budget": budget,
		"today":        today,
		"days":         days,
		"month_total":  month,
		"agents":       agents,
	})
}

// feedbackTopic tells everyone in #feedback where what they say goes: to
// whoever runs the service (GuildNet's team for guildnet.chat, an install's
// own people when self-hosted).
const feedbackTopic = "Anything said here goes to the people who run this service, who read it and may reply here. Tell them what works, what doesn't, and what you'd like."

// apiFeedback is what people (not agents or bots) have said lately in
// #feedback, for GuildNet's admins: the one channel whose words leave the
// network, as its topic says. Nothing from anywhere else.
func (r *Runner) apiFeedback(w http.ResponseWriter, req *http.Request) {
	type lineJSON struct {
		ID   string    `json:"id"`
		Nick string    `json:"nick"`
		Text string    `json:"text"`
		At   time.Time `json:"at"`
	}
	out := []lineJSON{}
	if ch := r.cfg.FeedbackChannel; ch != "" {
		for _, l := range r.chat.Recent(strings.ToLower(ch), archiveLines) {
			if r.isAgent(l.Nick) || isService(l.Nick) || strings.EqualFold(l.Nick, r.cfg.LedgerNick) || strings.EqualFold(l.Nick, guildServNick) {
				continue
			}
			id := l.MsgID
			if id == "" { // no id from the server: one made from the line itself
				sum := sha256.Sum256([]byte(l.At.UTC().Format(time.RFC3339Nano) + "\x00" + l.Nick + "\x00" + l.Text))
				id = "h-" + hex.EncodeToString(sum[:12])
			}
			out = append(out, lineJSON{ID: id, Nick: l.Nick, Text: l.Text, At: l.At.UTC()})
		}
	}
	apiJSON(w, http.StatusOK, map[string]any{"lines": out})
}

// lastActivity is when anyone last said something the runner heard.
func (r *Runner) lastActivity() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var last time.Time
	for _, t := range r.heard {
		if t.After(last) {
			last = t
		}
	}
	return last
}
