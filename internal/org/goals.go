// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"sort"
)

// Goal is something the company, or one of its teams, is aiming for: the
// guiding lights every agent reads. A team's goals are its objectives, and
// can say which of the company's goals they serve.
type Goal struct {
	ID   int64
	Team string // "" for the company's own goals
	Text string
	// Measure, optionally, is how it's measured, with where it should get to
	// and where it stands: "Weekly active customers", "100", "40". Free text,
	// so "4h" and "95%" work too.
	Measure, Target, Current string
	Serves                   int64 // for a team's objective, the company goal it serves; 0 for none
}

// Goals is where the company's description and everyone's goals are kept.
type Goals interface {
	// Company is the company's description: what it does, for whom, and how.
	Company(ctx context.Context) (string, error)
	SetCompany(ctx context.Context, description string) error
	// Goals are the company's goals, then teams', each in the order added.
	Goals(ctx context.Context) ([]Goal, error)
	AddGoal(ctx context.Context, g Goal) (Goal, error)
	UpdateGoal(ctx context.Context, g Goal) error
	DropGoal(ctx context.Context, id int64) error
}

func (m *Memory) Company(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.company, nil
}

func (m *Memory) SetCompany(_ context.Context, description string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.company = description
	return nil
}

func (m *Memory) Goals(context.Context) ([]Goal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Goal, 0, len(m.goals))
	for _, g := range m.goals {
		out = append(out, g)
	}
	sortGoals(out)
	return out, nil
}

// sortGoals puts the company's goals first, then teams' by team, each in the
// order added.
func sortGoals(gs []Goal) {
	sort.Slice(gs, func(i, j int) bool {
		if (gs[i].Team == "") != (gs[j].Team == "") {
			return gs[i].Team == ""
		}
		if key(gs[i].Team) != key(gs[j].Team) {
			return key(gs[i].Team) < key(gs[j].Team)
		}
		return gs[i].ID < gs[j].ID
	})
}

func (m *Memory) AddGoal(_ context.Context, g Goal) (Goal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.goals == nil {
		m.goals = map[int64]Goal{}
	}
	m.lastGoal++
	g.ID = m.lastGoal
	m.goals[g.ID] = g
	return g, nil
}

func (m *Memory) UpdateGoal(_ context.Context, g Goal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.goals[g.ID]; !ok {
		return ErrNotFound
	}
	m.goals[g.ID] = g
	return nil
}

func (m *Memory) DropGoal(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.goals[id]; !ok {
		return ErrNotFound
	}
	delete(m.goals, id)
	return nil
}
