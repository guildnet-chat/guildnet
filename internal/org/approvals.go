// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"errors"
	"sort"
	"time"
)

// ErrDecided means the approval was already approved, denied, or expired.
var ErrDecided = errors.New("already decided")

// Approval statuses.
const (
	Pending  = "pending"
	Approved = "approved"
	Denied   = "denied"
	Expired  = "expired"
)

// Approval is an agent waiting on a person before a risky action. It lives
// in the store so it survives restarts and weekends.
type Approval struct {
	ID        int64
	Agent     string // the agent's nick
	Approver  string // the account asked to decide
	Tool      string // the tool the agent wants to use
	Summary   string // what it wants to do, in words
	Channel   string // where the agent was working; "" for a DM
	Asker     string // who the agent was working for
	Resume    string // the agent platform's token for carrying on
	Status    string
	DecidedBy string
	Reason    string
	RaisedAt  time.Time
	ExpiresAt time.Time
	DecidedAt time.Time
	// Route is who's asked, in order, settled when it's raised; Approver
	// is who's being asked now, until MovesAt (zero: it stays with them).
	Route   []string
	MovesAt time.Time
	// resumed is when the agent carried on after the decision (memory
	// store; Postgres keeps resumed_at).
	resumed time.Time
}

// Approvals is where approvals are kept. Every Store is one.
type Approvals interface {
	RaiseApproval(ctx context.Context, a Approval) (Approval, error)
	Approval(ctx context.Context, id int64) (Approval, error)
	PendingApprovals(ctx context.Context) ([]Approval, error)
	DecideApproval(ctx context.Context, id int64, status, by, reason string, at time.Time) error
	// ApprovalsSince are the approvals raised since a time, decided or not.
	ApprovalsSince(ctx context.Context, since time.Time) ([]Approval, error)
	// MoveApproval asks the next person on a pending approval's route.
	MoveApproval(ctx context.Context, id int64, approver string, movesAt time.Time) error
	// Unresumed are the decided approvals (approved, denied or expired)
	// whose agent hasn't yet carried on: after a restart, they still need
	// to.
	Unresumed(ctx context.Context) ([]Approval, error)
	// ClaimResume marks a decided approval as carried on with, and reports
	// whether this was the first to: each is carried on with once. What the
	// agent carries on from isn't kept after.
	ClaimResume(ctx context.Context, id int64, at time.Time) (bool, error)
	// SetApprovalResume replaces what the agent carries on from (to seal
	// one kept before they were), until it has.
	SetApprovalResume(ctx context.Context, id int64, resume string) error
}

// Settings are the org's own settings, by name; unset ones read as "".
type Settings interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

func (m *Memory) RaiseApproval(_ context.Context, a Approval) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastApproval++
	a.ID, a.Status = m.lastApproval, Pending
	if m.approvals == nil {
		m.approvals = map[int64]Approval{}
	}
	m.approvals[a.ID] = a
	return a, nil
}

func (m *Memory) Approval(_ context.Context, id int64) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return a, nil
}

func (m *Memory) PendingApprovals(context.Context) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for _, a := range m.approvals {
		if a.Status == Pending {
			a.Resume = "" // as Postgres's: see listColumns
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) DecideApproval(_ context.Context, id int64, status, by, reason string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return ErrNotFound
	}
	if a.Status != Pending {
		return ErrDecided
	}
	a.Status, a.DecidedBy, a.Reason, a.DecidedAt = status, by, reason, at
	m.approvals[id] = a
	return nil
}

func (m *Memory) MoveApproval(_ context.Context, id int64, approver string, movesAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return ErrNotFound
	}
	if a.Status != Pending {
		return ErrDecided
	}
	a.Approver, a.MovesAt = approver, movesAt
	m.approvals[id] = a
	return nil
}

func (m *Memory) Unresumed(context.Context) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for _, a := range m.approvals {
		if a.Status != Pending && a.resumed.IsZero() {
			a.Resume = ""
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) ClaimResume(_ context.Context, id int64, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return false, ErrNotFound
	}
	if a.Status == Pending || !a.resumed.IsZero() {
		return false, nil
	}
	a.resumed, a.Resume = at, ""
	m.approvals[id] = a
	return true, nil
}

func (m *Memory) SetApprovalResume(_ context.Context, id int64, resume string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return ErrNotFound
	}
	if a.resumed.IsZero() { // carried on with already: nothing to keep
		a.Resume = resume
		m.approvals[id] = a
	}
	return nil
}

func (m *Memory) Setting(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings[key], nil
}

func (m *Memory) SetSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		m.settings = map[string]string{}
	}
	m.settings[key] = value
	return nil
}

func (m *Memory) ApprovalsSince(_ context.Context, since time.Time) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for _, a := range m.approvals {
		if !a.RaisedAt.Before(since) {
			a.Resume = ""
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
