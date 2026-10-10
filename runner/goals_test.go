// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"
)

// What the company and its teams are for: the company's description and
// goals, and each team's focus and objectives. They keep people and agents
// pointed at what matters, so every agent reads them when it wakes.

func goalID(t *testing.T, out map[string]any) float64 {
	t.Helper()
	id, ok := out["id"].(float64)
	if !ok || id == 0 {
		t.Fatalf("no goal id in %v", out)
	}
	return id
}

func TestTheOwnerDescribesTheCompanyAndSetsItsGoals(t *testing.T) {
	n := salesOrg(t)

	expectStatus(t, n, "method", http.StatusOK, "PATCH", "/v1/company", map[string]any{"description": "We make ovens for small bakeries."})
	out := expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/goals", map[string]any{
		"text": "Grow to 100 weekly customers", "measure": "Weekly active customers", "target": "100", "current": "40"})
	id := goalID(t, out)
	expectStatus(t, n, "method", http.StatusOK, "PATCH", "/v1/goals/"+ftoa(id), map[string]any{"current": "55"})

	company := expectStatus(t, n, "sam", http.StatusOK, "GET", "/v1/company", nil)
	if company["description"] != "We make ovens for small bakeries." {
		t.Errorf("description = %v", company["description"])
	}
	goals, _ := company["goals"].([]any)
	if len(goals) != 1 || goals[0].(map[string]any)["current"] != "55" || goals[0].(map[string]any)["text"] != "Grow to 100 weekly customers" {
		t.Errorf("goals = %v, want the one goal, now at 55", goals)
	}
}

func TestLeadsSetTheirTeamsFocusAndObjectives(t *testing.T) {
	n := salesOrg(t)
	grow := goalID(t, expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/goals", map[string]any{"text": "Grow to 100 weekly customers"}))

	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/teams/Sales", map[string]any{"focus": "Win bakeries that stay"})
	out := expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/goals", map[string]any{"team": "EMEA", "text": "Ten new bakeries in France", "serves": grow})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/goals", map[string]any{"team": "Engineering", "text": "Ship faster"})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/goals", map[string]any{"text": "A goal for the whole company"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/company", map[string]any{"description": "Dana's company now"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/teams/Engineering", map[string]any{"focus": "Sell things"})
	expectStatus(t, n, "dana", http.StatusBadRequest, "POST", "/v1/goals", map[string]any{"team": "EMEA", "text": "Serve nothing real", "serves": 999})

	if focus := teamIn(t, n, "Sales")["focus"]; focus != "Win bakeries that stay" {
		t.Errorf("Sales' focus = %v", focus)
	}
	if out["serves"] != grow {
		t.Errorf("EMEA's objective = %v, want it serving goal %v", out, grow)
	}
}

func TestAgentsReadTheCompanysGoalsAndTheirTeamsObjectives(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it")}
	n := startNetwork(t, brain, withAPI())
	owner := n.api("method")
	owner.call("PATCH", "/v1/company", map[string]any{"description": "We make ovens for small bakeries."})
	owner.call("POST", "/v1/goals", map[string]any{"text": "Grow to 100 weekly customers", "measure": "Weekly active customers", "target": "100", "current": "40"})
	owner.call("PATCH", "/v1/teams/Engineering", map[string]any{"focus": "Keep the ordering site fast"})
	owner.call("POST", "/v1/goals", map[string]any{"team": "Engineering", "text": "Pages load in under a second"})
	owner.call("POST", "/v1/goals", map[string]any{"team": "Marketing", "text": "Two posts a week"})

	n.human.say("#engineering", "kai: what should we work on?")
	n.human.expect("kai answering", from("kai", "#engineering"))

	goals := brain.lastRequest(t).Goals
	for _, want := range []string{"ovens for small bakeries", "Grow to 100 weekly customers", "40", "100", "Keep the ordering site fast", "Pages load in under a second"} {
		if !strings.Contains(goals, want) {
			t.Errorf("kai's goals should mention %q:\n%s", want, goals)
		}
	}
	if strings.Contains(goals, "Two posts a week") {
		t.Errorf("kai doesn't need Marketing's objectives:\n%s", goals)
	}
}

func TestGoalsFromIRC(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", `COMPANY DESCRIBE "We make ovens for small bakeries."`)
	n.human.expect("the description saved", guildServSaying("method", "ovens"))
	n.human.say("GuildServ", `GOAL ADD "Grow to 100 weekly customers" MEASURE "Weekly active customers" TARGET 100 CURRENT 40`)
	n.human.expect("the goal added", guildServSaying("method", "goal #1"))
	n.human.say("GuildServ", `GOAL ADD "Pages load in under a second" TEAM Engineering SERVES 1`)
	n.human.expect("the objective added", guildServSaying("method", "goal #2"))
	n.human.say("GuildServ", "GOAL SET 1 CURRENT 55")
	n.human.expect("progress saved", guildServSaying("method", "55"))
	n.human.say("GuildServ", "GOALS")

	n.human.expect("the company's goal", guildServSaying("method", "#1 Grow to 100 weekly customers (Weekly active customers: 55 of 100)"))
	n.human.expect("Engineering's objective", guildServSaying("method", "#2 Pages load in under a second, serving #1"))
}

// On a real Ergo, a team's focus becomes its channel's topic.
func TestATeamsFocusIsItsChannelTopic(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	var mu sync.Mutex
	topic := ""
	n.human.conn.AddCallback("332", func(e ircmsg.Message) { // RPL_TOPIC
		if len(e.Params) >= 3 && strings.EqualFold(e.Params[1], "#engineering") {
			mu.Lock()
			topic = e.Params[2]
			mu.Unlock()
		}
	})

	n.human.say("GuildServ", `TEAM SET Engineering FOCUS "Keep the ordering site fast"`)
	n.human.expect("the focus set", guildServSaying("method", "updated team"))

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n.human.conn.Send("TOPIC", "#engineering")
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		got := topic
		mu.Unlock()
		if got == "Keep the ordering site fast" {
			return
		}
	}
	t.Errorf("#engineering's topic = %q, want Engineering's focus", topic)
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
