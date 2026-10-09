// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"sort"
	"time"
)

// AccessLevel is a named set of abilities, and where they work. The owner
// changes the preset levels (Admin, Lead, Manager, Member) and makes their
// own; only what's been changed or made is stored.
type AccessLevel struct {
	Name      string
	Abilities []string // approve, hire, budgets, invite, arrange, spend, log
	// Scope is where the abilities work: "org" (everywhere), "led" (the teams
	// the holder leads), "reports" (everyone below the holder), "own" (the
	// holder's own team), or "teams" (the teams named in Teams). Teams inside
	// those count too.
	Scope string
	Teams []string
}

// AccessLevels is where changed and made access levels are kept. Names
// compare case-insensitively.
type AccessLevels interface {
	AccessLevels(ctx context.Context) ([]AccessLevel, error)
	SaveAccessLevel(ctx context.Context, a AccessLevel) error
	DropAccessLevel(ctx context.Context, name string) error
}

// LogEntry is one change to the org: who made it, what, and to whom.
type LogEntry struct {
	ID     int64
	At     time.Time
	Actor  string
	Action string // "hire", "set team", "give access", ...
	Target string // who or what it was done to
	Team   string // the team it happened in, so leads see their own; "" for the whole org
	Detail string // what changed, e.g. "budget $1.00 → $2.00"
}

// Log is the org's change log. Entries are only ever added.
type Log interface {
	AddLog(ctx context.Context, e LogEntry) error
	// Log is the latest limit entries, newest first.
	Log(ctx context.Context, limit int) ([]LogEntry, error)
}

func (m *Memory) AccessLevels(context.Context) ([]AccessLevel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sorted(m.access, func(a AccessLevel) string { return a.Name }), nil
}

func (m *Memory) SaveAccessLevel(_ context.Context, a AccessLevel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.access == nil {
		m.access = map[string]AccessLevel{}
	}
	m.access[key(a.Name)] = a
	return nil
}

func (m *Memory) DropAccessLevel(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.access[key(name)]; !ok {
		return ErrNotFound
	}
	delete(m.access, key(name))
	return nil
}

func (m *Memory) AddLog(_ context.Context, e LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.log) + 1)
	m.log = append(m.log, e)
	return nil
}

func (m *Memory) Log(_ context.Context, limit int) ([]LogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LogEntry, len(m.log))
	copy(out, m.log)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
