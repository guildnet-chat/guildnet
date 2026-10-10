// SPDX-License-Identifier: AGPL-3.0-only

// Package org holds a network's org chart: its teams, the humans in it, and
// the agents it employs. It's the start of the control plane; Postgres comes
// next.
package org

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/guildnet-chat/guildnet/config"
)

var (
	// ErrExists means something with that name is already in the org.
	ErrExists = errors.New("already exists")
	// ErrNotFound means nothing in the org has that name.
	ErrNotFound = errors.New("not found")
)

// Member is a human in the org, identified by their IRC account.
type Member struct {
	Account   string
	Name      string
	Role      string
	Team      string
	ReportsTo string // an account or an agent nick; "" for the owner
	Guest     bool
	// MonthlyCost is what this person costs their team per month, in USD,
	// so leads can weigh people against agents.
	MonthlyCost float64
	// Access is the access levels they've been given (Admin, or one the
	// owner made), on top of what leading a team or having reports gives.
	Access []string
}

// Team is a group of people and agents working toward something: a
// channel, the people in it, and its leads. A team can sit inside another.
type Team struct {
	Name    string
	Channel string
	Leads   []string // accounts or agent nicks
	Parent  string   // the team it's inside, or ""
	Focus   string   // what the team is for, in a line
	Repos   []string // the repos it works on; none means those of the team it's inside
	// Connectors are the connectors (Linear, Notion...) its agents use, by
	// ID; none means those of the team it's inside.
	Connectors []string
	// Chatter is how much of its agents' work the team's channel sees:
	// "quiet" (what they achieve), "summary" or "steps" (every step); ""
	// means that of the team it's inside, and at the top, a summary.
	Chatter string
	Private bool
	// MonthlyBudget, in USD, covers the team's humans and agents. 0 means
	// none set: only the owner can commit spend in the team.
	MonthlyBudget float64
}

// LedBy reports whether name is one of the team's leads.
func (t Team) LedBy(name string) bool {
	return name != "" && slices.ContainsFunc(t.Leads, func(l string) bool { return strings.EqualFold(l, name) })
}

// AgentMonthlyCost is what an agent commits against its team's budget: its
// daily spending cap for a 30-day month.
func AgentMonthlyCost(dailyCapUSD float64) float64 { return dailyCapUSD * 30 }

// Store is where the org chart lives. Names compare case-insensitively, and
// agent nicks and member accounts share one namespace.
type Store interface {
	Agents(ctx context.Context) ([]config.Persona, error)
	Agent(ctx context.Context, nick string) (config.Persona, error)
	Hire(ctx context.Context, p config.Persona) error
	Update(ctx context.Context, p config.Persona) error
	Fire(ctx context.Context, nick string) error

	Members(ctx context.Context) ([]Member, error)
	Member(ctx context.Context, account string) (Member, error)
	AddMember(ctx context.Context, m Member) error
	UpdateMember(ctx context.Context, m Member) error
	RemoveMember(ctx context.Context, account string) error

	Teams(ctx context.Context) ([]Team, error)
	Team(ctx context.Context, name string) (Team, error)
	CreateTeam(ctx context.Context, l Team) error
	UpdateTeam(ctx context.Context, l Team) error

	Approvals
	Desks
	AgentTokens
	AgentWebhooks
	ConnectorTools
	WorkLog
	AccessLevels
	Log
	Goals
	Settings
}

// Seed is an org chart to start from, usually loaded from guildnet.yaml.
type Seed struct {
	Agents  []config.Persona
	Members []Member
	Teams   []Team
	Company string // the company's description
	Goals   []Goal
}

// Memory is a Store that lives only as long as the process.
type Memory struct {
	mu             sync.Mutex
	agents         map[string]config.Persona
	members        map[string]Member
	teams          map[string]Team
	approvals      map[int64]Approval
	lastApproval   int64
	desks          map[string]Desk
	tokens         map[string]string // agent key -> its token's hash
	webhooks       map[string]webhook
	connectorTools map[string]ConnectorTool
	work           map[string][]Work // by agent, oldest first
	lastWork       int64
	access         map[string]AccessLevel
	log            []LogEntry
	company        string
	goals          map[int64]Goal
	lastGoal       int64
	settings       map[string]string
}

// NewMemory returns a Memory store holding seed.
func NewMemory(seed Seed) *Memory {
	m := &Memory{agents: map[string]config.Persona{}, members: map[string]Member{}, teams: map[string]Team{}}
	for _, p := range seed.Agents {
		m.agents[key(p.Nick)] = p
	}
	for _, x := range seed.Members {
		m.members[key(x.Account)] = x
	}
	for _, l := range seed.Teams {
		m.teams[key(l.Name)] = l
	}
	m.company = seed.Company
	for _, g := range seed.Goals {
		m.AddGoal(context.Background(), g)
	}
	return m
}

func key(s string) string { return strings.ToLower(s) }

// nameTaken reports whether an agent or member already uses name. Callers hold mu.
func (m *Memory) nameTaken(name string) bool {
	_, agent := m.agents[key(name)]
	_, member := m.members[key(name)]
	return agent || member
}

func (m *Memory) Agents(context.Context) ([]config.Persona, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sorted(m.agents, func(p config.Persona) string { return p.Nick }), nil
}

func (m *Memory) Agent(_ context.Context, nick string) (config.Persona, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.agents[key(nick)]
	if !ok {
		return config.Persona{}, ErrNotFound
	}
	return p, nil
}

func (m *Memory) Hire(_ context.Context, p config.Persona) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nameTaken(p.Nick) {
		return ErrExists
	}
	m.agents[key(p.Nick)] = p
	return nil
}

func (m *Memory) Update(_ context.Context, p config.Persona) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[key(p.Nick)]; !ok {
		return ErrNotFound
	}
	m.agents[key(p.Nick)] = p
	return nil
}

func (m *Memory) Fire(_ context.Context, nick string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[key(nick)]; !ok {
		return ErrNotFound
	}
	delete(m.agents, key(nick))
	delete(m.tokens, key(nick))
	delete(m.webhooks, key(nick))
	delete(m.work, key(nick))
	m.dropLead(nick)
	return nil
}

func (m *Memory) Members(context.Context) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sorted(m.members, func(x Member) string { return x.Account }), nil
}

func (m *Memory) Member(_ context.Context, account string) (Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.members[key(account)]
	if !ok {
		return Member{}, ErrNotFound
	}
	return x, nil
}

func (m *Memory) AddMember(_ context.Context, x Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nameTaken(x.Account) {
		return ErrExists
	}
	m.members[key(x.Account)] = x
	return nil
}

func (m *Memory) UpdateMember(_ context.Context, x Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[key(x.Account)]; !ok {
		return ErrNotFound
	}
	m.members[key(x.Account)] = x
	return nil
}

func (m *Memory) RemoveMember(_ context.Context, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[key(account)]; !ok {
		return ErrNotFound
	}
	delete(m.members, key(account))
	m.dropLead(account)
	return nil
}

func (m *Memory) Teams(context.Context) ([]Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sorted(m.teams, func(l Team) string { return l.Name }), nil
}

func (m *Memory) Team(_ context.Context, name string) (Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.teams[key(name)]
	if !ok {
		return Team{}, ErrNotFound
	}
	return l, nil
}

func (m *Memory) CreateTeam(_ context.Context, l Team) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.teams[key(l.Name)]; ok {
		return ErrExists
	}
	m.teams[key(l.Name)] = l
	return nil
}

func (m *Memory) UpdateTeam(_ context.Context, l Team) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.teams[key(l.Name)]; !ok {
		return ErrNotFound
	}
	m.teams[key(l.Name)] = l
	return nil
}

// dropLead stops name leading any team. Callers hold mu.
func (m *Memory) dropLead(name string) {
	for k, l := range m.teams {
		if l.LedBy(name) {
			l.Leads = slices.DeleteFunc(slices.Clone(l.Leads), func(x string) bool { return strings.EqualFold(x, name) })
			if len(l.Leads) == 0 {
				l.Leads = nil
			}
			m.teams[k] = l
		}
	}
}

func sorted[T any](items map[string]T, name func(T) string) []T {
	out := make([]T, 0, len(items))
	for _, x := range items {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return key(name(out[i])) < key(name(out[j])) })
	return out
}

// ReposFor is the repos a team works on: its own, or those of the nearest
// team it's inside that has some.
func ReposFor(ctx context.Context, s Store, team string) []string {
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[key(cur)]; {
		seen[key(cur)] = true
		t, err := s.Team(ctx, cur)
		if err != nil {
			return nil
		}
		if len(t.Repos) > 0 {
			return t.Repos
		}
		cur = t.Parent
	}
	return nil
}
