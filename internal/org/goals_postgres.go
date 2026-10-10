// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) Company(ctx context.Context) (string, error) {
	var d string
	err := p.pool.QueryRow(ctx, `SELECT description FROM company WHERE id = 1`).Scan(&d)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return d, err
}

func (p *Postgres) SetCompany(ctx context.Context, description string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO company (id, description) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET description = $1, updated_at = now()`, description)
	return err
}

const goalColumns = `id, team, text, measure, target, current, serves`

func (p *Postgres) Goals(ctx context.Context) ([]Goal, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+goalColumns+` FROM goals`)
	if err != nil {
		return nil, err
	}
	gs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Goal, error) {
		var g Goal
		err := r.Scan(&g.ID, &g.Team, &g.Text, &g.Measure, &g.Target, &g.Current, &g.Serves)
		return g, err
	})
	sortGoals(gs)
	return gs, err
}

func (p *Postgres) AddGoal(ctx context.Context, g Goal) (Goal, error) {
	err := p.pool.QueryRow(ctx, `INSERT INTO goals (team, text, measure, target, current, serves) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		g.Team, g.Text, g.Measure, g.Target, g.Current, g.Serves).Scan(&g.ID)
	return g, err
}

func (p *Postgres) UpdateGoal(ctx context.Context, g Goal) error {
	tag, err := p.pool.Exec(ctx, `UPDATE goals SET team = $2, text = $3, measure = $4, target = $5, current = $6, serves = $7,
		updated_at = now() WHERE id = $1`, g.ID, g.Team, g.Text, g.Measure, g.Target, g.Current, g.Serves)
	return notFoundIfNone(tag.RowsAffected(), err)
}

func (p *Postgres) DropGoal(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM goals WHERE id = $1`, id)
	return notFoundIfNone(tag.RowsAffected(), err)
}
