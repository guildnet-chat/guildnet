// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"encoding/json"
	"time"
)

// Work is one wake-up of an agent, as its page in the console shows it:
// where, for whom, the steps it took, how it ended, and what it cost.
type Work struct {
	ID    int64
	Agent string
	Team  string
	Where string // a channel, or "a DM" (never who with)
	// Asker and Asked are who woke it and what they said, in a channel;
	// empty for a DM.
	Asker, Asked      string
	Started, Finished time.Time
	Steps             []WorkStep
	Outcome           string // "answered", "passed", "waiting on approval #7", "failed: ..."
	Cost              float64
}

// WorkStep is one step of the work.
type WorkStep struct {
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Result  bool      `json:"result,omitempty"` // what a step achieved
}

// WorkKept is how many wake-ups are kept per agent.
const WorkKept = 50

// WorkLog keeps agents' recent work. Every Store is one.
type WorkLog interface {
	AddWork(ctx context.Context, w Work) error
	// Work is an agent's latest limit wake-ups, newest first.
	Work(ctx context.Context, agent string, limit int) ([]Work, error)
}

func (m *Memory) AddWork(_ context.Context, w Work) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastWork++
	w.ID = m.lastWork
	w.Steps = append([]WorkStep(nil), w.Steps...)
	if m.work == nil {
		m.work = map[string][]Work{}
	}
	list := append(m.work[key(w.Agent)], w)
	if len(list) > WorkKept {
		list = list[len(list)-WorkKept:]
	}
	m.work[key(w.Agent)] = list
	return nil
}

func (m *Memory) Work(_ context.Context, agent string, limit int) ([]Work, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.work[key(agent)]
	var out []Work
	for i := len(list) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, list[i])
	}
	return out, nil
}

func (p *Postgres) AddWork(ctx context.Context, w Work) error {
	steps, err := json.Marshal(w.Steps)
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_work (agent, team, place, asker, asked, started_at, finished_at, steps, outcome, cost_usd)
		SELECT key, $2, $3, $4, $5, $6, $7, $8, $9, $10 FROM agents WHERE key = $1`,
		key(w.Agent), w.Team, w.Where, w.Asker, w.Asked, w.Started, w.Finished, steps, w.Outcome, w.Cost); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_work WHERE agent = $1 AND id NOT IN
		(SELECT id FROM agent_work WHERE agent = $1 ORDER BY id DESC LIMIT $2)`, key(w.Agent), WorkKept); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Work(ctx context.Context, agent string, limit int) ([]Work, error) {
	rows, err := p.pool.Query(ctx, `SELECT w.id, a.nick, w.team, w.place, w.asker, w.asked, w.started_at, w.finished_at, w.steps, w.outcome, w.cost_usd
		FROM agent_work w JOIN agents a ON a.key = w.agent WHERE w.agent = $1 ORDER BY w.id DESC LIMIT $2`, key(agent), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Work
	for rows.Next() {
		var w Work
		var steps []byte
		if err := rows.Scan(&w.ID, &w.Agent, &w.Team, &w.Where, &w.Asker, &w.Asked, &w.Started, &w.Finished, &steps, &w.Outcome, &w.Cost); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(steps, &w.Steps); err != nil {
			return nil, err
		}
		w.Started, w.Finished = w.Started.UTC(), w.Finished.UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}
