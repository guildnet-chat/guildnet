// SPDX-License-Identifier: AGPL-3.0-only

package workweek_test

import (
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/workweek"
)

// Deadlines count only working time: nobody works weekends, so a request
// that's waiting on a person doesn't run out while they're off.

var edmonton, _ = time.LoadLocation("America/Edmonton")

func at(s string) time.Time {
	t, err := time.ParseInLocation("Mon 2006-01-02 15:04", s, edmonton)
	if err != nil {
		panic(err)
	}
	return t
}

func TestADeadlineSkipsTheWeekend(t *testing.T) {
	cases := []struct{ raised, want string }{
		{"Wed 2026-09-23 10:00", "Thu 2026-09-24 10:00"}, // midweek: a day later
		{"Fri 2026-09-25 17:00", "Mon 2026-09-28 17:00"}, // Friday: over the weekend
		{"Fri 2026-09-25 23:30", "Mon 2026-09-28 23:30"}, // late Friday still skips it
		{"Sat 2026-09-26 09:00", "Tue 2026-09-29 00:00"}, // raised on a weekend: the clock starts Monday
		{"Sun 2026-09-27 22:00", "Tue 2026-09-29 00:00"},
	}
	for _, c := range cases {
		got := workweek.After(at(c.raised), 24*time.Hour, edmonton)
		if !got.Equal(at(c.want)) {
			t.Errorf("24 working hours from %s = %s, want %s", c.raised, got.In(edmonton).Format("Mon 2006-01-02 15:04"), c.want)
		}
	}
}

func TestLongDeadlinesSkipEveryWeekend(t *testing.T) {
	got := workweek.After(at("Thu 2026-09-24 12:00"), 72*time.Hour, edmonton)
	// Thu noon + 12h = Fri midnight; Fri 24h = Sat midnight -> skip to Mon; Mon 24h; Tue 12h.
	if want := at("Tue 2026-09-29 12:00"); !got.Equal(want) {
		t.Errorf("72 working hours from Thu noon = %s, want Tue noon", got.In(edmonton).Format("Mon 2006-01-02 15:04"))
	}
}

func TestDaylightSavingDoesntShiftTheDeadline(t *testing.T) {
	// Alberta falls back on Sun 2026-11-01. A Friday request still expires
	// Monday at the same wall-clock time.
	got := workweek.After(at("Fri 2026-10-30 09:00"), 24*time.Hour, edmonton)
	if want := at("Mon 2026-11-02 09:00"); !got.Equal(want) {
		t.Errorf("got %s, want Mon 09:00", got.In(edmonton).Format("Mon 2006-01-02 15:04"))
	}
}
