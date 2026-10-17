// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// A private team's details (its lead, budget, repositories, connectors and
// objectives) are for the people in it, the leads above it and the owner.
// Someone else in the org learns the team is there, and that it's private,
// and nothing more.
func TestAPrivateTeamsDetailsAreHiddenFromPeopleOutsideIt(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	for _, cmd := range []string{"TEAM CREATE Leadership LEAD kai PRIVATE", "TEAM SET Leadership BUDGET 500", "TEAM CREATE Sales"} {
		n.human.say("GuildServ", cmd)
		n.human.expect(cmd, fromGuildServ("method"))
	}
	n.human.say("GuildServ", `GOAL ADD "Buy Acme quietly" TEAM Leadership`)
	n.human.expect("the objective added", guildServSaying("method", "Buy Acme quietly"))
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Seller" TEAM Sales`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	dana := n.joinAs("dana")
	secret := func(m heard) bool {
		return fromGuildServ("dana")(m) && (containsFold(m.text, "Buy Acme") || containsFold(m.text, "$500") || containsFold(m.text, "led by kai"))
	}

	dana.say("GuildServ", "TEAMS")
	dana.expect("the team list", guildServSaying("dana", "Leadership (#leadership, private)"))
	dana.say("GuildServ", "GOALS")
	dana.expect("the objectives", fromGuildServ("dana"))
	dana.say("GuildServ", "GOALS Leadership")
	dana.expect("an answer about Leadership's objectives", fromGuildServ("dana"))
	dana.say("GuildServ", "BUDGET Leadership")
	dana.expect("an answer about Leadership's budget", fromGuildServ("dana"))

	dana.expectQuiet("a private team's details", time.Second, secret)

	// The owner sees them.
	n.human.say("GuildServ", "BUDGET Leadership")
	n.human.expect("the owner seeing the budget", guildServSaying("method", "$500"))
}

// The same through the API the console uses: the org chart, a team's
// numbers and budget, and the company's goals.
func TestAPrivateTeamsDetailsAreHiddenInTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	owner, dana := n.api("method"), n.api("dana")
	for _, call := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/teams", map[string]any{"name": "Leadership", "budget": 500, "private": true}},
		{"/v1/teams", map[string]any{"name": "Sales"}},
		{"/v1/members", map[string]any{"account": "dana", "role": "Seller", "team": "Sales"}},
	} {
		if status, out := owner.call("POST", call.path, call.body); status != http.StatusCreated {
			t.Fatalf("POST %s: %d %v", call.path, status, out)
		}
	}
	n.human.say("GuildServ", `GOAL ADD "Buy Acme quietly" TEAM Leadership`)
	n.human.expect("the objective added", guildServSaying("method", "Buy Acme quietly"))

	_, org := dana.call("GET", "/v1/org", nil)
	for _, team := range org["teams"].([]any) {
		team := team.(map[string]any)
		if team["name"] == "Leadership" && (team["monthly_budget"] != 0.0 || team["private"] != true) {
			t.Errorf("dana sees Leadership as %v: want that it's there and private, and no more", team)
		}
	}
	for _, path := range []string{"/v1/teams/Leadership/budget", "/v1/teams/Leadership/stats"} {
		if status, out := dana.call("GET", path, nil); status != http.StatusForbidden {
			t.Errorf("dana's GET %s: %d %v, want 403", path, status, out)
		}
	}
	if _, company := dana.call("GET", "/v1/company", nil); strings.Contains(strings.ToLower(fmt.Sprint(company)), "buy acme") {
		t.Errorf("dana sees the private team's objective: %v", company)
	}
	if status, out := owner.call("GET", "/v1/teams/Leadership/stats", nil); status != http.StatusOK {
		t.Errorf("the owner's stats: %d %v", status, out)
	}
}
