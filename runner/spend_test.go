// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// The owner's spend view: what the network's agents have spent today
// against its daily cap, each day this month, and each agent's share, with
// its model and daily cap. Only for the owner: it's the company's money.

func TestTheOwnerSeesSpendByDayAndByAgent(t *testing.T) {
	brain := &fakeBrain{reply: func(agent.Request) string { return "on it" }, cost: 0.25}
	n := startNetwork(t, brain, withAPI())
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	earlier := first.Format("2006-01-02") != now.Format("2006-01-02")
	if earlier {
		writeLedgerDay(t, n.cfg.DataDir, first, 1.5)
	}
	n.human.say("#engineering", "kai: build it")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "on it"))

	status, out := n.api("method").call("GET", "/v1/spend", nil)

	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	if out["daily_budget"] != 5.0 {
		t.Errorf("daily_budget = %v, want 5", out["daily_budget"])
	}
	today := out["today"].(map[string]any)
	if today["total"] != 0.25 || today["calls"] != 1.0 || today["by_agent"].(map[string]any)["kai"] != 0.25 {
		t.Errorf("today = %v, want kai's $0.25 in one call", today)
	}
	days := out["days"].([]any)
	wantMonth, wantDays := 0.25, 1
	if earlier {
		wantMonth, wantDays = 1.75, 2
	}
	if len(days) != wantDays {
		t.Errorf("days = %v, want %d", days, wantDays)
	}
	if got, _ := out["month_total"].(float64); got < wantMonth-1e-9 || got > wantMonth+1e-9 {
		t.Errorf("month_total = %v, want %v", out["month_total"], wantMonth)
	}
	agents := map[string]map[string]any{}
	for _, a := range out["agents"].([]any) {
		m := a.(map[string]any)
		agents[m["nick"].(string)] = m
	}
	kai := agents["kai"]
	if kai == nil || kai["model"] != "claude-sonnet-5" || kai["today"] != 0.25 || kai["team"] != "Engineering" {
		t.Errorf("kai = %v", kai)
	}
	if agents["vera"] == nil || agents["vera"]["today"] != 0.0 {
		t.Errorf("vera, who spent nothing, should be listed at $0: %v", agents["vera"])
	}
}

// Spend by scope: the owner (and anyone who may see spend across the org)
// sees the whole network; a lead sees their teams' agents, with totals over
// just those; someone who may see no spend is refused.
func TestEachPersonSeesSpendForTheAgentsTheyMaySee(t *testing.T) {
	brain := &fakeBrain{reply: func(agent.Request) string { return "on it" }, cost: 0.25}
	n := startNetwork(t, brain, withAPI())
	withSales(t, n) // dana leads Sales
	owner := n.api("method")
	owner.call("POST", "/v1/members", map[string]any{"account": "sam", "team": "Engineering"})
	owner.call("POST", "/v1/people/sam/promote", nil)
	owner.call("POST", "/v1/members", map[string]any{"account": "eve", "team": "Sales"})
	owner.call("POST", "/v1/staff", map[string]any{"nick": "rex", "team": "Sales", "budget": 1})
	n.human.say("#engineering", "kai: build it")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "on it"))

	nicks := func(out map[string]any) []string {
		var got []string
		for _, a := range out["agents"].([]any) {
			got = append(got, a.(map[string]any)["nick"].(string))
		}
		return got
	}
	status, bySam := n.api("sam").call("GET", "/v1/spend", nil)
	// Not vera: she leads Engineering too, and sam reports to her.
	if status != http.StatusOK || !slices.Equal(nicks(bySam), []string{"kai"}) || bySam["scope"] != "part" {
		t.Errorf("sam, a lead of Engineering: %d, agents %v, scope %v", status, bySam["agents"], bySam["scope"])
	}
	if today := bySam["today"].(map[string]any); today["total"] != 0.25 {
		t.Errorf("sam's today = %v, want Engineering's $0.25", today)
	}
	status, byDana := n.api("dana").call("GET", "/v1/spend", nil)
	if status != http.StatusOK || !slices.Equal(nicks(byDana), []string{"rex"}) {
		t.Errorf("dana, Sales' lead: %d, agents %v, want just rex", status, byDana["agents"])
	}
	if today := byDana["today"].(map[string]any); today["total"] != 0.0 || byDana["month_total"] != 0.0 {
		t.Errorf("dana's totals = %v and %v, want nothing of Engineering's", today, byDana["month_total"])
	}
	if _, byOwner := owner.call("GET", "/v1/spend", nil); byOwner["scope"] != "org" || byOwner["daily_budget"] != 5.0 {
		t.Errorf("the owner: scope %v, daily budget %v, want the whole org and its $5 cap", byOwner["scope"], byOwner["daily_budget"])
	}
	if status, _ := n.api("eve").call("GET", "/v1/spend", nil); status != http.StatusForbidden {
		t.Errorf("eve, who may see no spend: %d, want 403", status)
	}
}
