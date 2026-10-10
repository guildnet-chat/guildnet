// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"math"
	"net/http"

	"github.com/guildnet-chat/guildnet/charts"
)

// The spend view: what the network's agents have spent on the owner's
// model providers' keys, today against the daily cap, each day this month, and each
// agent's share. The runner answers with what the person may see: the
// whole network for the owner and admins; otherwise the agents they may see
// spend for, against their caps added up.

type spendView struct {
	Whole                     bool // the whole network, not just some agents
	DailyBudget, Today, Month float64
	TodayPct                  int
	Calls                     int
	Agents                    []agentSpend
	DaysChart, MonthChart     charts.Chart
}

type agentSpend struct {
	Nick, Team, Model         string
	DailyBudget, Today, Month float64
}

func (p *Platform) spendPage(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Scope       string  `json:"scope"`
		DailyBudget float64 `json:"daily_budget"`
		Today       struct {
			Total float64 `json:"total"`
			Calls int     `json:"calls"`
		} `json:"today"`
		Days []struct {
			Day     string             `json:"day"`
			Total   float64            `json:"total"`
			ByAgent map[string]float64 `json:"by_agent"`
		} `json:"days"`
		MonthTotal float64 `json:"month_total"`
		Agents     []struct {
			Nick        string  `json:"nick"`
			Team        string  `json:"team"`
			Model       string  `json:"model"`
			DailyBudget float64 `json:"daily_budget"`
			Today       float64 `json:"today"`
			Month       float64 `json:"month"`
		} `json:"agents"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/spend", nil, &out); err != nil {
		p.render(w, r, statusOf(err), "spend", withError{Err: messageOf(err)})
		return
	}
	v := spendView{Whole: out.Scope != "part", DailyBudget: out.DailyBudget, Today: out.Today.Total, Month: out.MonthTotal, Calls: out.Today.Calls}
	v.TodayPct = percent(v.Today, v.DailyBudget)
	byAgent := map[string]map[string]float64{}
	for _, d := range out.Days {
		byAgent[d.Day] = d.ByAgent
	}
	v.DaysChart, v.MonthChart = spendCharts(p.opts.Now(), v.DailyBudget, byAgent)
	for _, a := range out.Agents {
		v.Agents = append(v.Agents, agentSpend{Nick: a.Nick, Team: a.Team, Model: a.Model, DailyBudget: a.DailyBudget, Today: a.Today, Month: a.Month})
	}
	p.render(w, r, http.StatusOK, "spend", withError{View: v})
}

// percent is part of whole, rounded, 0 if there's no whole.
func percent(part, whole float64) int {
	if whole <= 0 {
		return 0
	}
	return int(math.Round(part / whole * 100))
}
