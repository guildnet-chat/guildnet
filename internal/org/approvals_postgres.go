// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const approvalColumns = `id, agent, approver, tool, summary, channel, asker, resume, status,
	decided_by, reason, raised_at, expires_at, decided_at, route, moves_at`

// listColumns are approvalColumns for lists: without what the agent needs
// to carry on (its whole conversation, which can be large), read only for
// the approval it's carrying on from.
const listColumns = `id, agent, approver, tool, summary, channel, asker, '' AS resume, status,
	decided_by, reason, raised_at, expires_at, decided_at, route, moves_at`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	var decided, moves *time.Time
	err := row.Scan(&a.ID, &a.Agent, &a.Approver, &a.Tool, &a.Summary, &a.Channel, &a.Asker, &a.Resume,
		&a.Status, &a.DecidedBy, &a.Reason, &a.RaisedAt, &a.ExpiresAt, &decided, &a.Route, &moves)
	a.Route = none(a.Route)
	if moves != nil {
		a.MovesAt = *moves
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if decided != nil {
		a.DecidedAt = *decided
	}
	return a, err
}

func (p *Postgres) RaiseApproval(ctx context.Context, a Approval) (Approval, error) {
	return scanApproval(p.pool.QueryRow(ctx, `INSERT INTO approvals
		(agent, approver, tool, summary, channel, asker, resume, raised_at, expires_at, route, moves_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+approvalColumns,
		a.Agent, a.Approver, a.Tool, a.Summary, a.Channel, a.Asker, a.Resume, a.RaisedAt, a.ExpiresAt, list(a.Route), orNull(a.MovesAt)))
}

func (p *Postgres) Approval(ctx context.Context, id int64) (Approval, error) {
	return scanApproval(p.pool.QueryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = $1`, id))
}

func (p *Postgres) PendingApprovals(ctx context.Context) ([]Approval, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+listColumns+` FROM approvals WHERE status = 'pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approval, error) { return scanApproval(r) })
}

func (p *Postgres) DecideApproval(ctx context.Context, id int64, status, by, reason string, at time.Time) error {
	tag, err := p.pool.Exec(ctx, `UPDATE approvals SET status = $2, decided_by = $3, reason = $4, decided_at = $5
		WHERE id = $1 AND status = 'pending'`, id, status, by, reason, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := p.Approval(ctx, id); err != nil {
			return err
		}
		return ErrDecided
	}
	return nil
}

func (p *Postgres) MoveApproval(ctx context.Context, id int64, approver string, movesAt time.Time) error {
	tag, err := p.pool.Exec(ctx, `UPDATE approvals SET approver = $2, moves_at = $3 WHERE id = $1 AND status = 'pending'`,
		id, approver, orNull(movesAt))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := p.Approval(ctx, id); err != nil {
			return err
		}
		return ErrDecided
	}
	return nil
}

func (p *Postgres) Unresumed(ctx context.Context) ([]Approval, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+listColumns+` FROM approvals
		WHERE status <> 'pending' AND resumed_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approval, error) { return scanApproval(r) })
}

func (p *Postgres) ClaimResume(ctx context.Context, id int64, at time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE approvals SET resumed_at = $2
		WHERE id = $1 AND status <> 'pending' AND resumed_at IS NULL`, id, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// orNull stores a zero time as NULL.
func orNull(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (p *Postgres) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := p.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (p *Postgres) SetSetting(ctx context.Context, key, value string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = now()`, key, value)
	return err
}

func (p *Postgres) ApprovalsSince(ctx context.Context, since time.Time) ([]Approval, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+listColumns+` FROM approvals WHERE raised_at >= $1 ORDER BY id`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approval, error) { return scanApproval(r) })
}
