// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/guildnet-chat/guildnet/config"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres is a Store in a Postgres database. The schema is migrated on
// open, and seed is imported only into a database that's never been seeded.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to the database at url.
func OpenPostgres(ctx context.Context, url string, seed Seed) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	// One runner per network, all on one small Postgres: a few connections
	// each (the org is kept in memory between changes), let go when idle.
	cfg.MaxConns, cfg.MaxConnIdleTime = 3, 2*time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := migrate(pool); err != nil {
		pool.Close()
		return nil, err
	}
	p := &Postgres{pool: pool}
	if err := p.seed(ctx, seed); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Close disconnects.
func (p *Postgres) Close() { p.pool.Close() }

func migrate(pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}

func (p *Postgres) seed(ctx context.Context, s Seed) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO org_seeded (id) VALUES (1) ON CONFLICT DO NOTHING`)
		if err != nil || tag.RowsAffected() == 0 {
			return err // already seeded, or failed
		}
		for _, a := range s.Agents {
			if err := hire(ctx, tx, a); err != nil {
				return fmt.Errorf("seeding agent %s: %w", a.Nick, err)
			}
		}
		for _, m := range s.Members {
			if err := addMember(ctx, tx, m); err != nil {
				return fmt.Errorf("seeding member %s: %w", m.Account, err)
			}
		}
		for _, l := range s.Teams {
			if err := createTeam(ctx, tx, l); err != nil {
				return fmt.Errorf("seeding team %s: %w", l.Name, err)
			}
		}
		if s.Company != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO company (id, description) VALUES (1, $1)`, s.Company); err != nil {
				return fmt.Errorf("seeding the company's description: %w", err)
			}
		}
		for _, g := range s.Goals {
			if _, err := tx.Exec(ctx, `INSERT INTO goals (team, text, measure, target, current, serves) VALUES ($1, $2, $3, $4, $5, $6)`,
				g.Team, g.Text, g.Measure, g.Target, g.Current, g.Serves); err != nil {
				return fmt.Errorf("seeding goal %q: %w", g.Text, err)
			}
		}
		return nil
	})
}

const agentColumns = `nick, name, role, team, reports_to, model, effort, daily_budget_usd, channels, prompt, platform, daily_wakes`

func scanAgent(row pgx.Row) (config.Persona, error) {
	var a config.Persona
	err := row.Scan(&a.Nick, &a.Name, &a.Role, &a.Team, &a.ReportsTo, &a.Model, &a.Effort, &a.DailyBudgetUSD, &a.Channels, &a.Prompt, &a.Platform, &a.DailyWakes)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (p *Postgres) Agents(ctx context.Context) ([]config.Persona, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+agentColumns+` FROM agents ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (config.Persona, error) { return scanAgent(r) })
}

func (p *Postgres) Agent(ctx context.Context, nick string) (config.Persona, error) {
	return scanAgent(p.pool.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents WHERE key = $1`, key(nick)))
}

func (p *Postgres) Hire(ctx context.Context, a config.Persona) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return hire(ctx, tx, a) })
}

// claim takes name for kind, or returns ErrExists if it's taken.
func claim(ctx context.Context, tx pgx.Tx, name, kind string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO org_names (key, kind) VALUES ($1, $2) ON CONFLICT DO NOTHING`, key(name), kind)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	return nil
}

func hire(ctx context.Context, tx pgx.Tx, a config.Persona) error {
	if err := claim(ctx, tx, a.Nick, "agent"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO agents (key, `+agentColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		key(a.Nick), a.Nick, a.Name, a.Role, a.Team, a.ReportsTo, a.Model, a.Effort, a.DailyBudgetUSD, channels(a.Channels), a.Prompt, a.Platform, a.DailyWakes)
	return err
}

func (p *Postgres) Update(ctx context.Context, a config.Persona) error {
	tag, err := p.pool.Exec(ctx, `UPDATE agents SET name = $2, role = $3, team = $4, reports_to = $5, model = $6,
		effort = $7, daily_budget_usd = $8, channels = $9, prompt = $10, platform = $11, daily_wakes = $12, updated_at = now() WHERE key = $1`,
		key(a.Nick), a.Name, a.Role, a.Team, a.ReportsTo, a.Model, a.Effort, a.DailyBudgetUSD, channels(a.Channels), a.Prompt, a.Platform, a.DailyWakes)
	return notFoundIfNone(tag.RowsAffected(), err)
}

func (p *Postgres) Fire(ctx context.Context, nick string) error {
	return p.release(ctx, nick, "agent")
}

// release frees a name and stops it leading any team.
func (p *Postgres) release(ctx context.Context, name, kind string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM org_names WHERE key = $1 AND kind = $2`, key(name), kind)
		if err := notFoundIfNone(tag.RowsAffected(), err); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE teams SET leads = array(SELECT l FROM unnest(leads) AS l WHERE lower(l) <> $1),
			updated_at = now() WHERE $1 = ANY(array(SELECT lower(l) FROM unnest(leads) AS l))`, key(name))
		return err
	})
}

const memberColumns = `account, name, role, team, reports_to, guest, monthly_cost_usd, access`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.Account, &m.Name, &m.Role, &m.Team, &m.ReportsTo, &m.Guest, &m.MonthlyCost, &m.Access)
	m.Access = none(m.Access)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (p *Postgres) Members(ctx context.Context) ([]Member, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+memberColumns+` FROM members ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) { return scanMember(r) })
}

func (p *Postgres) Member(ctx context.Context, account string) (Member, error) {
	return scanMember(p.pool.QueryRow(ctx, `SELECT `+memberColumns+` FROM members WHERE key = $1`, key(account)))
}

func (p *Postgres) AddMember(ctx context.Context, m Member) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return addMember(ctx, tx, m) })
}

func addMember(ctx context.Context, tx pgx.Tx, m Member) error {
	if err := claim(ctx, tx, m.Account, "member"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO members (key, `+memberColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		key(m.Account), m.Account, m.Name, m.Role, m.Team, m.ReportsTo, m.Guest, m.MonthlyCost, list(m.Access))
	return err
}

func (p *Postgres) UpdateMember(ctx context.Context, m Member) error {
	tag, err := p.pool.Exec(ctx, `UPDATE members SET name = $2, role = $3, team = $4, reports_to = $5, guest = $6,
		monthly_cost_usd = $7, access = $8, updated_at = now() WHERE key = $1`,
		key(m.Account), m.Name, m.Role, m.Team, m.ReportsTo, m.Guest, m.MonthlyCost, list(m.Access))
	return notFoundIfNone(tag.RowsAffected(), err)
}

func (p *Postgres) RemoveMember(ctx context.Context, account string) error {
	return p.release(ctx, account, "member")
}

const teamColumns = `name, channel, leads, parent, focus, private, monthly_budget_usd, repos, connectors, chatter`

func scanTeam(row pgx.Row) (Team, error) {
	var l Team
	err := row.Scan(&l.Name, &l.Channel, &l.Leads, &l.Parent, &l.Focus, &l.Private, &l.MonthlyBudget, &l.Repos, &l.Connectors, &l.Chatter)
	l.Leads, l.Repos, l.Connectors = none(l.Leads), none(l.Repos), none(l.Connectors)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (p *Postgres) Teams(ctx context.Context) ([]Team, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+teamColumns+` FROM teams ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) { return scanTeam(r) })
}

func (p *Postgres) Team(ctx context.Context, name string) (Team, error) {
	return scanTeam(p.pool.QueryRow(ctx, `SELECT `+teamColumns+` FROM teams WHERE key = $1`, key(name)))
}

func (p *Postgres) CreateTeam(ctx context.Context, l Team) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return createTeam(ctx, tx, l) })
}

func createTeam(ctx context.Context, tx pgx.Tx, l Team) error {
	tag, err := tx.Exec(ctx, `INSERT INTO teams (key, `+teamColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT DO NOTHING`,
		key(l.Name), l.Name, l.Channel, list(l.Leads), l.Parent, l.Focus, l.Private, l.MonthlyBudget, list(l.Repos), list(l.Connectors), l.Chatter)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	return nil
}

func (p *Postgres) UpdateTeam(ctx context.Context, l Team) error {
	tag, err := p.pool.Exec(ctx, `UPDATE teams SET channel = $2, leads = $3, parent = $4, focus = $5, private = $6, monthly_budget_usd = $7, repos = $8,
		connectors = $9, chatter = $10, updated_at = now() WHERE key = $1`, key(l.Name), l.Channel, list(l.Leads), l.Parent, l.Focus, l.Private, l.MonthlyBudget, list(l.Repos), list(l.Connectors), l.Chatter)
	return notFoundIfNone(tag.RowsAffected(), err)
}

func notFoundIfNone(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// channels stores nil as an empty list, so it reads back the same.
func channels(chans []string) []string { return list(chans) }

// list stores nil as an empty list (the column can't be null); none reads an
// empty list back as nil, as the memory store keeps it.
func list(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func none(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	return xs
}
