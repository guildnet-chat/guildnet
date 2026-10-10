// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/guildnet-chat/guildnet/config"
)

// Cached is a Store that keeps the people, teams and access levels in
// memory between changes. Deciding who may do what reads them over and over
// (a console page can ask hundreds of times); they change rarely. A runner
// is the only one changing its org, through here, so any change made here
// clears what's kept, and the next read loads it afresh.
type Cached struct {
	Store
	mu      sync.Mutex
	gen     int // counts changes: what was read before one isn't kept
	members []Member
	teams   []Team
	levels  []AccessLevel
	have    struct{ members, teams, levels bool }
}

// NewCached keeps s's people, teams and access levels in memory.
func NewCached(s Store) *Cached { return &Cached{Store: s} }

// changed clears everything kept after a change, whether it worked or not:
// any change can touch more than it says (removing someone takes them off
// the teams they led).
func (c *Cached) changed(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.members, c.teams, c.levels = nil, nil, nil
	c.have.members, c.have.teams, c.have.levels = false, false, false
	return err
}

// Copies are handed out: a caller changing what it got (adding a lead, say)
// mustn't change what's kept.

func cloneMember(m Member) Member {
	m.Access = slices.Clone(m.Access)
	return m
}

func cloneTeam(t Team) Team {
	t.Leads, t.Repos, t.Connectors = slices.Clone(t.Leads), slices.Clone(t.Repos), slices.Clone(t.Connectors)
	return t
}

func cloneLevel(l AccessLevel) AccessLevel {
	l.Abilities, l.Teams = slices.Clone(l.Abilities), slices.Clone(l.Teams)
	return l
}

func cloneAll[T any](xs []T, clone func(T) T) []T {
	if xs == nil {
		return nil
	}
	out := make([]T, len(xs))
	for i, x := range xs {
		out[i] = clone(x)
	}
	return out
}

// kept is what's kept of one kind, or it read from the store and kept (if
// nothing changed meanwhile). Callers get copies.
func kept[T any](c *Cached, have *bool, slot *[]T, read func() ([]T, error), clone func(T) T) ([]T, error) {
	c.mu.Lock()
	if *have {
		defer c.mu.Unlock()
		return cloneAll(*slot, clone), nil
	}
	gen := c.gen
	c.mu.Unlock()
	xs, err := read()
	if err != nil {
		return nil, err
	}
	xs = cloneAll(xs, clone)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen {
		*slot, *have = xs, true
	}
	return cloneAll(xs, clone), nil
}

func (c *Cached) Members(ctx context.Context) ([]Member, error) {
	return kept(c, &c.have.members, &c.members, func() ([]Member, error) { return c.Store.Members(ctx) }, cloneMember)
}

func (c *Cached) Member(ctx context.Context, account string) (Member, error) {
	ms, err := c.Members(ctx)
	if err != nil {
		return Member{}, err
	}
	if i := slices.IndexFunc(ms, func(m Member) bool { return strings.EqualFold(m.Account, account) }); i >= 0 {
		return ms[i], nil
	}
	return Member{}, ErrNotFound
}

func (c *Cached) Teams(ctx context.Context) ([]Team, error) {
	return kept(c, &c.have.teams, &c.teams, func() ([]Team, error) { return c.Store.Teams(ctx) }, cloneTeam)
}

func (c *Cached) Team(ctx context.Context, name string) (Team, error) {
	ts, err := c.Teams(ctx)
	if err != nil {
		return Team{}, err
	}
	if i := slices.IndexFunc(ts, func(t Team) bool { return strings.EqualFold(t.Name, name) }); i >= 0 {
		return ts[i], nil
	}
	return Team{}, ErrNotFound
}

func (c *Cached) AccessLevels(ctx context.Context) ([]AccessLevel, error) {
	return kept(c, &c.have.levels, &c.levels, func() ([]AccessLevel, error) { return c.Store.AccessLevels(ctx) }, cloneLevel)
}

// Every change to people, teams, agents (who may lead teams) or access
// levels clears what's kept.

func (c *Cached) Hire(ctx context.Context, p config.Persona) error {
	return c.changed(c.Store.Hire(ctx, p))
}
func (c *Cached) Update(ctx context.Context, p config.Persona) error {
	return c.changed(c.Store.Update(ctx, p))
}
func (c *Cached) Fire(ctx context.Context, nick string) error {
	return c.changed(c.Store.Fire(ctx, nick))
}
func (c *Cached) AddMember(ctx context.Context, m Member) error {
	return c.changed(c.Store.AddMember(ctx, m))
}
func (c *Cached) UpdateMember(ctx context.Context, m Member) error {
	return c.changed(c.Store.UpdateMember(ctx, m))
}
func (c *Cached) RemoveMember(ctx context.Context, account string) error {
	return c.changed(c.Store.RemoveMember(ctx, account))
}
func (c *Cached) CreateTeam(ctx context.Context, t Team) error {
	return c.changed(c.Store.CreateTeam(ctx, t))
}
func (c *Cached) UpdateTeam(ctx context.Context, t Team) error {
	return c.changed(c.Store.UpdateTeam(ctx, t))
}
func (c *Cached) SaveAccessLevel(ctx context.Context, a AccessLevel) error {
	return c.changed(c.Store.SaveAccessLevel(ctx, a))
}
func (c *Cached) DropAccessLevel(ctx context.Context, name string) error {
	return c.changed(c.Store.DropAccessLevel(ctx, name))
}
