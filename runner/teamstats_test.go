// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"testing"
	"time"
)

// A team's numbers: who's in it (and the teams inside it), approvals waiting
// and how long they take to decide, when its agents last said anything, and,
// for those who may see spend there, what it spent this month.

func TestATeamsNumbers(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
	n := startNetwork(t, wantsToMerge(), withClock(clock), withAPI())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	clock.Advance(90 * time.Minute)
	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
	n.api("method").call("POST", "/v1/members", map[string]any{"account": "sam", "team": "Engineering"})

	status, out := n.api("method").call("GET", "/v1/teams/Engineering/stats", nil)

	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	if out["agents"] != 2.0 || out["humans"] != 1.0 {
		t.Errorf("agents %v, humans %v; want vera and kai, and sam", out["agents"], out["humans"])
	}
	if out["waiting"] != 0.0 || out["decided"] != 1.0 || out["median_minutes"] != 90.0 {
		t.Errorf("approvals: waiting %v, decided %v, median %v minutes; want none waiting, one decided in 90", out["waiting"], out["decided"], out["median_minutes"])
	}
	last, _ := out["last_said"].(map[string]any)
	if last["kai"] == nil {
		t.Errorf("last_said = %v, want when kai last said something", out["last_said"])
	}
	if _, ok := out["spend_month"]; !ok {
		t.Error("the owner may see what Engineering spent")
	}
	if _, bySam := n.api("sam").call("GET", "/v1/teams/Engineering/stats", nil); bySam["spend_month"] != nil {
		t.Errorf("sam may not see Engineering's spend: %v", bySam["spend_month"])
	}
}
