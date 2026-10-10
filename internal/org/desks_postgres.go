// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) Desk(ctx context.Context, nick string) (Desk, error) {
	var d Desk
	err := p.pool.QueryRow(ctx, `SELECT nick, agent_id, agent_version, session_id, cost_cents FROM desks WHERE key = $1`,
		key(nick)).Scan(&d.Nick, &d.AgentID, &d.AgentVersion, &d.SessionID, &d.CostCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (p *Postgres) SaveDesk(ctx context.Context, d Desk) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO desks (key, nick, agent_id, agent_version, session_id, cost_cents)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (key) DO UPDATE SET agent_id = $3, agent_version = $4, session_id = $5, cost_cents = $6, updated_at = now()`,
		key(d.Nick), d.Nick, d.AgentID, d.AgentVersion, d.SessionID, d.CostCents)
	return err
}
