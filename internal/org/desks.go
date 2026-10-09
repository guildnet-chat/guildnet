// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"strings"
)

// Desk is where an agent works on an agent platform: its agent config there
// and its long-lived session (workspace), and what that session has cost so
// far, so each wake-up can be charged the difference.
type Desk struct {
	Nick         string
	AgentID      string
	AgentVersion int64
	SessionID    string
	CostCents    int64
}

// Desks is where desks are kept. Every Store is one.
type Desks interface {
	Desk(ctx context.Context, nick string) (Desk, error)
	SaveDesk(ctx context.Context, d Desk) error
}

func (m *Memory) Desk(_ context.Context, nick string) (Desk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.desks[strings.ToLower(nick)]
	if !ok {
		return Desk{}, ErrNotFound
	}
	return d, nil
}

func (m *Memory) SaveDesk(_ context.Context, d Desk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.desks == nil {
		m.desks = map[string]Desk{}
	}
	m.desks[strings.ToLower(d.Nick)] = d
	return nil
}
