// SPDX-License-Identifier: AGPL-3.0-only

// Package charts describes charts for the console's pages: a page puts the
// description in as JSON, and its charts.js draws it with Chart.js. The
// site's Content Security Policy allows that: it's data, not a script.
package charts

import "time"

// Chart is one chart: labels along one axis, and series of values.
type Chart struct {
	Kind   string   `json:"kind"` // "bar" (stacked), "line", or "hbar" (horizontal, stacked)
	Labels []string `json:"labels"`
	Series []Series `json:"series"`
	Money  bool     `json:"money,omitempty"` // values are dollars
	Unit   string   `json:"unit,omitempty"`  // or in this unit, e.g. MB
}

// Series are a chart's values of one kind.
type Series struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`           // nil where there's no value (a gap)
	Line   bool       `json:"line,omitempty"`   // a line over bars
	Dashed bool       `json:"dashed,omitempty"` // an estimate, or a limit
	Right  bool       `json:"right,omitempty"`  // on its own axis, on the right
	Quiet  bool       `json:"quiet,omitempty"`  // drawn in a muted colour
}

// Val is a value, for Series.Values.
func Val(v float64) *float64 { return &v }

// MonthDays are the days of now's UTC month up to today, at midnight UTC.
func MonthDays(now time.Time) []time.Time {
	now = now.UTC()
	var out []time.Time
	for day := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC); !day.After(now); day = day.AddDate(0, 0, 1) {
		out = append(out, day)
	}
	return out
}
