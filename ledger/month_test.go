// SPDX-License-Identifier: AGPL-3.0-only

package ledger_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/ledger"
)

// The month so far, day by day, for the owner's spend view: each day's file,
// and today as the ledger has it now. Other months' days aren't in it.
func TestThisMonthIsEachDaySoFarWithTodayAsItStands(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	write := func(day time.Time, total float64, byAgent map[string]float64) {
		b, _ := json.Marshal(map[string]any{"day": day.Format("2006-01-02"), "total_usd": total, "by_agent_usd": byAgent, "calls": 2})
		if err := os.WriteFile(filepath.Join(dir, "ledger-"+day.Format("2006-01-02")+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	earlier := first.Format("2006-01-02") != now.Format("2006-01-02")
	if earlier {
		write(first, 1.5, map[string]float64{"kai": 1, "rey": 0.5})
	}
	write(first.AddDate(0, 0, -1), 9, map[string]float64{"kai": 9}) // last month's last day
	l, err := ledger.Open(dir, 5)
	if err != nil {
		t.Fatal(err)
	}
	l.RecordCost("kai", 0.25)

	days := l.ThisMonth()

	want := 1
	if earlier {
		want = 2
	}
	if len(days) != want {
		t.Fatalf("days = %+v, want %d (this month's only)", days, want)
	}
	today := days[len(days)-1]
	if today.Day != now.Format("2006-01-02") || today.Total != 0.25 || today.Calls != 1 || today.ByAgent["kai"] != 0.25 {
		t.Errorf("today = %+v, want kai's $0.25 in one call", today)
	}
	if earlier && (days[0].Day != first.Format("2006-01-02") || days[0].ByAgent["rey"] != 0.5) {
		t.Errorf("the 1st = %+v", days[0])
	}
	if got, want := l.Month(), 0.25+map[bool]float64{true: 1.5}[earlier]; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("Month() = %v, want %v", got, want)
	}
}
