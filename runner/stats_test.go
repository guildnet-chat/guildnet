// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// GuildNet's own admins see how each network is used, in numbers only: how
// big it is, what it has spent, and when it was last active. Never who is in
// it, what they said, or what its agents do.

func TestANetworksStatsAreNumbersOnly(t *testing.T) {
	brain := &fakeBrain{reply: func(agent.Request) string { return "on it" }, cost: 0.25}
	n := startNetwork(t, brain, withAPI())
	// Spend earlier this month, and last month (which doesn't count).
	now := time.Now().UTC()
	earlier := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if earlier.Format("2006-01-02") != now.Format("2006-01-02") {
		writeLedgerDay(t, n.cfg.DataDir, earlier, 1.5)
	} else {
		t.Log("it's the first of the month: only today's spend counts")
	}
	writeLedgerDay(t, n.cfg.DataDir, earlier.AddDate(0, -1, 0), 40)

	n.human.say("#engineering", "kai: build it")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "on it"))

	c := n.api("")
	c.header = false // the platform asks as itself, for GuildNet's admins
	status, out := c.call("GET", "/v1/stats", nil)

	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	for k, want := range map[string]float64{"people": 1, "agents": 2, "teams": 2, "spend_today": 0.25, "calls_today": 1, "daily_budget": 5} {
		if out[k] != want {
			t.Errorf("%s = %v, want %v", k, out[k], want)
		}
	}
	wantMonth := 0.25
	if earlier.Format("2006-01-02") != now.Format("2006-01-02") {
		wantMonth = 1.75
	}
	if got, _ := out["spend_month"].(float64); got < wantMonth-1e-9 || got > wantMonth+1e-9 {
		t.Errorf("spend_month = %v, want %v (this month's days, not last month's)", out["spend_month"], wantMonth)
	}
	days, _ := out["days"].([]any)
	if len(days) == 0 {
		t.Fatalf("days = %v, want this month's", out["days"])
	}
	todayJSON := days[len(days)-1].(map[string]any)
	if todayJSON["day"] != now.Format("2006-01-02") || todayJSON["total"] != 0.25 || todayJSON["calls"] != 1.0 || len(todayJSON) != 3 {
		t.Errorf("today in days = %v, want just its day, total and calls", todayJSON)
	}
	last, err := time.Parse(time.RFC3339, stringOf(out["last_activity"]))
	if err != nil || time.Since(last) > time.Minute {
		t.Errorf("last_activity = %v, want just now", out["last_activity"])
	}
	b, _ := json.Marshal(out)
	for _, private := range []string{"kai", "vera", "method", "Engineering", "build it", "on it", "#"} {
		if strings.Contains(string(b), private) {
			t.Errorf("the stats give away %q: %s", private, b)
		}
	}
}

func TestANetworksStatsNeedItsSecret(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	for _, token := range []string{"", "network-b-secret"} {
		c := n.api("")
		c.header, c.token = false, token
		if status, out := c.call("GET", "/v1/stats", nil); status != http.StatusUnauthorized {
			t.Errorf("token %q: status %d (%v), want 401", token, status, out)
		}
	}
}

func TestANetworkNobodyHasSpokenInHasNoLastActivity(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	c := n.api("")
	c.header = false

	_, out := c.call("GET", "/v1/stats", nil)

	if out["last_activity"] != nil {
		t.Errorf("last_activity = %v, want none", out["last_activity"])
	}
}

// writeLedgerDay leaves a day's ledger file, as the ledger does.
func writeLedgerDay(t *testing.T, dir string, day time.Time, total float64) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"day": day.Format("2006-01-02"), "total_usd": total, "by_agent_usd": map[string]float64{"kai": total}, "calls": 3})
	if err := os.WriteFile(filepath.Join(dir, "ledger-"+day.Format("2006-01-02")+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
