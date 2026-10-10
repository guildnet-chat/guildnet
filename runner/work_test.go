// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"strings"
	"testing"
)

// Each wake-up is kept for the agent's page in the console: where, who asked
// and what, its steps, how it ended, and what it cost. Seen by the agent's
// team, its manager, and whoever may see the team's log; the cost only by
// those who may see its spend. A DM is only "a DM".

func workOf(t *testing.T, n *network, actor, agent string) (int, []map[string]any) {
	t.Helper()
	status, out := n.api(actor).call("GET", "/v1/staff/"+agent+"/work", nil)
	var list []map[string]any
	all, _ := out["work"].([]any)
	for _, x := range all {
		list = append(list, x.(map[string]any))
	}
	return status, list
}

func TestAnAgentsWorkIsKeptForItsPage(t *testing.T) {
	brain := working("runs go test ./...", "reads a.go", "=> opened PR #3")
	brain.cost = 0.25
	n := startNetwork(t, brain, withAPI())
	withSales(t, n) // dana leads Sales, not Engineering

	n.human.say("#engineering", "kai: open a PR for the login fix")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests pass"))
	n.human.say("kai", "a private question")
	n.human.expect("kai's reply in the DM", fromContaining("kai", "method", "tests pass"))

	status, work := workOf(t, n, "method", "kai")
	if status != http.StatusOK || len(work) != 2 {
		t.Fatalf("kai's work: %d %v", status, work)
	}
	dm, channel := work[0], work[1]
	if channel["where"] != "#engineering" || channel["asker"] != "method" || !strings.Contains(channel["asked"].(string), "login fix") ||
		channel["outcome"] != "answered" || channel["cost"] != 0.25 || len(channel["steps"].([]any)) != 3 {
		t.Errorf("the channel's wake-up: %v", channel)
	}
	if step := channel["steps"].([]any)[2].(map[string]any); step["summary"] != "opened PR #3" || step["result"] != true {
		t.Errorf("its last step: %v", step)
	}
	if dm["where"] != "a DM" || dm["asker"] != nil || dm["asked"] != nil {
		t.Errorf("a DM says only that it was one: %v", dm)
	}

	if status, _ := workOf(t, n, "dana", "kai"); status != http.StatusForbidden {
		t.Errorf("dana, outside Engineering: %d, want 403", status)
	}
}

func TestOnlyThoseWhoMaySeeSpendSeeWhatWorkCost(t *testing.T) {
	brain := working("runs go test ./...")
	brain.cost = 0.25
	n := startNetwork(t, brain, withAPI())
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))

	n.human.say("#engineering", "kai: run the tests")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests pass"))

	status, work := workOf(t, n, "sam", "kai")
	if status != http.StatusOK || len(work) != 1 {
		t.Fatalf("sam, in Engineering: %d %v", status, work)
	}
	if _, shown := work[0]["cost"]; shown {
		t.Errorf("sam may not see spend, but saw %v", work[0]["cost"])
	}
}
