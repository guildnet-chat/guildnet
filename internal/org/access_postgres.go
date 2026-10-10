// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) AccessLevels(ctx context.Context) ([]AccessLevel, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, abilities, scope, teams FROM access_levels ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccessLevel, error) {
		var a AccessLevel
		err := r.Scan(&a.Name, &a.Abilities, &a.Scope, &a.Teams)
		a.Abilities, a.Teams = none(a.Abilities), none(a.Teams)
		return a, err
	})
}

func (p *Postgres) SaveAccessLevel(ctx context.Context, a AccessLevel) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO access_levels (key, name, abilities, scope, teams) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key) DO UPDATE SET name = $2, abilities = $3, scope = $4, teams = $5, updated_at = now()`,
		key(a.Name), a.Name, list(a.Abilities), a.Scope, list(a.Teams))
	return err
}

func (p *Postgres) DropAccessLevel(ctx context.Context, name string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM access_levels WHERE key = $1`, key(name))
	return notFoundIfNone(tag.RowsAffected(), err)
}

func (p *Postgres) AddLog(ctx context.Context, e LogEntry) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO org_log (at, actor, action, target, team, detail) VALUES ($1, $2, $3, $4, $5, $6)`,
		e.At, e.Actor, e.Action, e.Target, e.Team, e.Detail)
	return err
}

func (p *Postgres) Log(ctx context.Context, limit int) ([]LogEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := p.pool.Query(ctx, `SELECT id, at, actor, action, target, team, detail FROM org_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogEntry, error) {
		var e LogEntry
		err := r.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &e.Team, &e.Detail)
		return e, err
	})
}
