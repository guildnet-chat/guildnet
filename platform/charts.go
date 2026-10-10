// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"sort"
	"time"

	"github.com/guildnet-chat/guildnet/charts"
)

// Charts: a page describes what to draw (charts), and static/charts.js
// draws it with Chart.js (vendored, like htmx).

// chartBox is a chart as a page places it: its name, what it shows (for
// screen readers), and its data.
type chartBox struct {
	Name, Label string
	Data        charts.Chart
}

func chartOf(name, label string, c any) chartBox {
	box := chartBox{Name: name, Label: label}
	switch c := c.(type) {
	case charts.Chart:
		box.Data = c
	case *charts.Chart:
		if c != nil {
			box.Data = *c
		}
	}
	return box
}

// spendCharts are the owner's spend charts: each day this month by agent
// against the daily cap, and the month so far with where it's heading.
func spendCharts(now time.Time, dailyBudget float64, byDay map[string]map[string]float64) (days, month charts.Chart) {
	val := charts.Val
	span := charts.MonthDays(now)
	days = charts.Chart{Kind: "bar", Money: true}
	var agents []string
	seen := map[string]bool{}
	for _, d := range span {
		days.Labels = append(days.Labels, d.Format("Jan 2"))
		for a, v := range byDay[d.Format("2006-01-02")] {
			if v > 0 && !seen[a] {
				seen[a] = true
				agents = append(agents, a)
			}
		}
	}
	sort.Strings(agents)
	for _, a := range agents {
		s := charts.Series{Name: a}
		for _, d := range span {
			s.Values = append(s.Values, val(byDay[d.Format("2006-01-02")][a]))
		}
		days.Series = append(days.Series, s)
	}
	capLine := charts.Series{Name: "Daily cap", Line: true, Dashed: true, Quiet: true}
	for range span {
		capLine.Values = append(capLine.Values, val(dailyBudget))
	}
	days.Series = append(days.Series, capLine)

	// The whole month: spent so far up to today, then at this pace to the
	// end, and the most it could be if every day reached the cap.
	first := span[0]
	end := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	month = charts.Chart{Kind: "line", Money: true}
	spent := charts.Series{Name: "Spent so far"}
	pace := charts.Series{Name: "At this pace", Dashed: true}
	most := charts.Series{Name: "If every day hit the cap", Dashed: true, Quiet: true}
	total := 0.0
	for i := range end {
		day := first.AddDate(0, 0, i)
		month.Labels = append(month.Labels, day.Format("Jan 2"))
		most.Values = append(most.Values, val(dailyBudget*float64(i+1)))
		if i < len(span) {
			for _, v := range byDay[day.Format("2006-01-02")] {
				total += v
			}
			spent.Values = append(spent.Values, val(total))
		} else {
			spent.Values = append(spent.Values, nil)
		}
	}
	perDay := total / float64(len(span))
	for i := range end {
		if i < len(span)-1 {
			pace.Values = append(pace.Values, nil)
			continue
		}
		pace.Values = append(pace.Values, val(total+perDay*float64(i+1-len(span))))
	}
	month.Series = []charts.Series{spent, pace, most}
	return days, month
}
