// SPDX-License-Identifier: AGPL-3.0-only

// Package ledger tracks model spend per agent per UTC day and enforces budgets.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/models"
)

// Usage is the token usage of one API call. Output includes any reasoning
// tokens, which bill as output.
type Usage struct {
	Input, Output, CacheWrite, CacheRead int64
}

// Cost returns the USD cost of u on model, at today's prices.
func Cost(model string, u Usage) float64 { return CostAt(model, u, time.Now()) }

// CostAt returns the USD cost of u on model at t's prices, from the model
// catalogue. A model it can't price is charged at the dearest listed rate,
// so the budget errs on the side of stopping early.
func CostAt(model string, u Usage, t time.Time) float64 {
	var p models.Price
	if m, ok := models.Find(model); ok && m.Priced {
		p = m.Price(t)
	} else {
		p = dearest(t)
	}
	in := float64(u.Input)*p.Input + float64(u.CacheWrite)*p.CacheWrite + float64(u.CacheRead)*p.CacheRead
	return (in + float64(u.Output)*p.Output) / 1e6
}

// dearest is the most any listed model charges for each kind of token.
func dearest(t time.Time) models.Price {
	var d models.Price
	for _, m := range models.All {
		p := m.Price(t)
		d.Input, d.Output = max(d.Input, p.Input), max(d.Output, p.Output)
		d.CacheRead, d.CacheWrite = max(d.CacheRead, p.CacheRead), max(d.CacheWrite, p.CacheWrite)
	}
	return d
}

// Thresholds are the budget percentages that trigger a warning in #ops.
var Thresholds = []int{50, 80, 100}

// Ledger is safe for concurrent use.
type Ledger struct {
	mu     sync.Mutex
	dir    string
	budget float64
	state  state
	closed bool // nothing more is written to dir
}

// Close stops the ledger writing to its directory, once any write under
// way is done: its runner is stopping. Spend is still counted in memory.
func (l *Ledger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}

type state struct {
	Day     string             `json:"day"`
	Total   float64            `json:"total_usd"`
	ByAgent map[string]float64 `json:"by_agent_usd"`
	Calls   int                `json:"calls"`
	Warned  map[int]bool       `json:"warned"`
	// Via is the part of the day's spend that went some particular way (the
	// model gateway), within the totals above.
	Via map[string]viaTotal `json:"via,omitempty"`
}

type viaTotal struct {
	Total float64 `json:"total_usd"`
	Calls int     `json:"calls"`
}

// Open loads today's ledger from dir, or starts a fresh one.
func Open(dir string, dailyBudget float64) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Ledger{dir: dir, budget: dailyBudget}
	l.rollover()
	b, err := os.ReadFile(l.path())
	if err == nil {
		if err := json.Unmarshal(b, &l.state); err != nil {
			return nil, fmt.Errorf("%s: %w", l.path(), err)
		}
	}
	return l, nil
}

// Record adds a call's cost, worked out from its token usage, and returns
// it, plus any thresholds it crossed.
func (l *Ledger) Record(agent, model string, u Usage) (cost float64, crossed []int) {
	return l.RecordCost(agent, Cost(model, u))
}

// RecordCost adds a charge the platform reported itself (which can include
// more than tokens, like sandbox time), plus any thresholds it crossed.
func (l *Ledger) RecordCost(agent string, cost float64) (float64, []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	var crossed []int
	l.state.Total += cost
	l.state.ByAgent[agent] += cost
	l.state.Calls++
	pct := l.state.Total / l.budget * 100
	for _, t := range Thresholds {
		if pct >= float64(t) && !l.state.Warned[t] {
			l.state.Warned[t] = true
			crossed = append(crossed, t)
		}
	}
	l.save()
	return cost, crossed
}

// RecordVia is RecordCost for a call that went some particular way (the
// model gateway), which is also counted apart.
func (l *Ledger) RecordVia(agent, via string, cost float64) (float64, []int) {
	cost, crossed := l.RecordCost(agent, cost)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.Via == nil {
		l.state.Via = map[string]viaTotal{}
	}
	v := l.state.Via[via]
	v.Total += cost
	v.Calls++
	l.state.Via[via] = v
	l.save()
	return cost, crossed
}

// Via is today's spend that went via, and how many calls it took.
func (l *Ledger) Via(via string) (float64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	v := l.state.Via[via]
	return v.Total, v.Calls
}

// Left is what agent may still spend today: the least of what's left of
// the network's budget and of its own (none means no cap of its own).
func (l *Ledger) Left(agent string, agentBudget float64) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	left := l.budget - l.state.Total
	if agentBudget > 0 {
		left = min(left, agentBudget-l.state.ByAgent[agent])
	}
	return max(left, 0)
}

// Allowed reports whether agent may make another call today, and why not.
func (l *Ledger) Allowed(agent string, agentBudget float64) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	if l.state.Total >= l.budget {
		return false, fmt.Sprintf("the network's daily budget ($%.2f) is used up", l.budget)
	}
	if agentBudget > 0 && l.state.ByAgent[agent] >= agentBudget {
		return false, fmt.Sprintf("%s's daily budget ($%.2f) is used up", agent, agentBudget)
	}
	return true, ""
}

// Summary is a one-line spend report for IRC.
func (l *Ledger) Summary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	agents := make([]string, 0, len(l.state.ByAgent))
	for a := range l.state.ByAgent {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	parts := make([]string, 0, len(agents))
	for _, a := range agents {
		parts = append(parts, fmt.Sprintf("%s $%.3f", a, l.state.ByAgent[a]))
	}
	by := "no spend yet"
	if len(parts) > 0 {
		by = strings.Join(parts, ", ")
	}
	return fmt.Sprintf("spend %s: $%.3f of $%.2f (%.0f%%), %d calls | %s",
		l.state.Day, l.state.Total, l.budget, l.state.Total/l.budget*100, l.state.Calls, by)
}

// Budget returns the daily network budget in USD.
func (l *Ledger) Budget() float64 { return l.budget }

// Today is today's spend and how many calls it took.
func (l *Ledger) Today() (float64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	return l.state.Total, l.state.Calls
}

// DaySpend is one UTC day's spend.
type DaySpend struct {
	Day     string // YYYY-MM-DD
	Total   float64
	Calls   int
	ByAgent map[string]float64
}

// ThisMonth is each day of this UTC month so far that has a ledger, oldest
// first, with today as it stands now.
func (l *Ledger) ThisMonth() []DaySpend {
	l.mu.Lock()
	l.rollover()
	today := DaySpend{Day: l.state.Day, Total: l.state.Total, Calls: l.state.Calls, ByAgent: map[string]float64{}}
	for a, v := range l.state.ByAgent {
		today.ByAgent[a] = v
	}
	l.mu.Unlock()
	var days []DaySpend
	files, _ := filepath.Glob(filepath.Join(l.dir, "ledger-"+today.Day[:len("2006-01-")]+"??.json"))
	sort.Strings(files)
	for _, f := range files {
		if filepath.Base(f) == "ledger-"+today.Day+".json" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s state
		if json.Unmarshal(b, &s) == nil && s.Day < today.Day {
			days = append(days, DaySpend{Day: s.Day, Total: s.Total, Calls: s.Calls, ByAgent: s.ByAgent})
		}
	}
	return append(days, today)
}

// Month is this UTC month's spend so far.
func (l *Ledger) Month() float64 {
	total := 0.0
	for _, d := range l.ThisMonth() {
		total += d.Total
	}
	return total
}

// Detail is one call's usage and cost, logged alongside the daily totals in
// state so spend can be broken down by model and by conversation, not just
// by agent.
type Detail struct {
	Time  time.Time `json:"time"`
	Agent string    `json:"agent"`
	Model string    `json:"model"`
	Where string    `json:"where"` // channel, or a DM description
	Usage Usage     `json:"usage"`
	Cost  float64   `json:"cost_usd"`
	// Prices is the catalogue version that priced it (models.Version).
	Prices string `json:"prices,omitempty"`
}

// AppendDetail appends one call's usage to today's detail log
// (calls-YYYY-MM-DD.jsonl in the ledger directory). Errors are non-fatal,
// same as save: the in-memory totals still enforce the budget.
func (l *Ledger) AppendDetail(d Detail) {
	if d.Time.IsZero() {
		d.Time = time.Now().UTC()
	}
	if d.Prices == "" {
		d.Prices = models.Version
	}
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	// Held while writing, so Close waits for it.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.rollover()
	f, err := os.OpenFile(filepath.Join(l.dir, "calls-"+l.state.Day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// rollover resets the state when the UTC day changes. Callers hold mu.
func (l *Ledger) rollover() {
	today := time.Now().UTC().Format("2006-01-02")
	if l.state.Day != today {
		l.state = state{Day: today, ByAgent: map[string]float64{}, Warned: map[int]bool{}}
	}
}

func (l *Ledger) path() string {
	return filepath.Join(l.dir, "ledger-"+l.state.Day+".json")
}

// save writes the state to disk. Callers hold mu. Errors are non-fatal: the
// in-memory ledger still enforces the budget for this run.
func (l *Ledger) save() {
	if l.closed {
		return
	}
	b, err := json.MarshalIndent(l.state, "", "  ")
	if err != nil {
		return
	}
	tmp := l.path() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, l.path())
	}
}
