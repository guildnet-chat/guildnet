// SPDX-License-Identifier: AGPL-3.0-only

// Package workweek does deadline arithmetic in working time. Nobody works
// weekends, so time spent waiting on a person over a weekend doesn't count.
package workweek

import "time"

// After returns the moment d of working time has passed since start, in
// loc's calendar. Saturdays and Sundays don't count; something raised on a
// weekend starts counting on Monday at midnight.
func After(start time.Time, d time.Duration, loc *time.Location) time.Time {
	t := skipWeekend(start.In(loc))
	for {
		y, m, day := t.Date()
		midnight := time.Date(y, m, day+1, 0, 0, 0, 0, loc)
		if left := midnight.Sub(t); d <= left {
			return t.Add(d)
		} else {
			d -= left
		}
		t = skipWeekend(midnight)
	}
}

// skipWeekend moves a weekend time to the following Monday at midnight.
func skipWeekend(t time.Time) time.Time {
	switch t.Weekday() {
	case time.Saturday:
		y, m, d := t.Date()
		return time.Date(y, m, d+2, 0, 0, 0, 0, t.Location())
	case time.Sunday:
		y, m, d := t.Date()
		return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
	}
	return t
}
