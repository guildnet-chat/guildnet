// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/guildnet-chat/guildnet/models"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres keeps the platform's own data: networks (with their secrets
// sealed), sessions, and the waitlist. It's the platform's database only;
// each network's data lives in that network's own database.
type Postgres struct {
	pool   *pgxpool.Pool
	sealer Sealer
}

// OpenPostgres connects and migrates the schema.
func OpenPostgres(ctx context.Context, url string, sealer Sealer) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	// Every console and the broker share these: more than pgx's default on
	// one CPU, and let go when idle.
	cfg.MaxConns, cfg.MaxConnIdleTime = 8, 5*time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		pool.Close()
		return nil, err
	}
	if err := goose.Up(db, "migrations"); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrating the platform database: %w", err)
	}
	s := &Postgres{pool: pool, sealer: sealer}
	if err := s.upgrade(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("upgrading the platform database: %w", err)
	}
	return s, nil
}

func (s *Postgres) Close() { s.pool.Close() }

// sealed is a network's secrets, sealed for storage.
type sealed struct{ runnerToken, ergoToken, chatKey, modelKeys []byte }

// networkSecrets are where each of a network's secrets is kept, and so
// bound: its column, the network, and the address it's sent to (so changing
// the address in the database doesn't send it elsewhere).
func networkSecrets(name, runnerURL, ergoURL, chatURL string) (runner, ergo, chat, keys []byte) {
	name = strings.ToLower(name)
	return aad("networks", "runner_token", name, runnerURL), aad("networks", "ergo_token", name, ergoURL),
		aad("networks", "chat_key", name, chatURL), aad("networks", "model_keys", name)
}

func (s *Postgres) seal(n Network) (sealed, error) {
	var out sealed
	runner, ergo, chat, keys := networkSecrets(n.Name, n.RunnerURL, n.ErgoURL, n.ChatURL)
	for _, f := range []struct {
		plain    string
		aad      []byte
		into     *[]byte
		optional bool
	}{{n.RunnerToken, runner, &out.runnerToken, false}, {n.ErgoToken, ergo, &out.ergoToken, false}, {n.ChatKey, chat, &out.chatKey, true}, {keysJSON(n.Keys), keys, &out.modelKeys, true}} {
		if f.optional && f.plain == "" {
			continue
		}
		b, err := s.sealer.Seal([]byte(f.plain), f.aad)
		if err != nil {
			return out, err
		}
		*f.into = b
	}
	return out, nil
}

// networkColumns are a network's columns, in the order networkValues and
// scanNetwork use.
const networkColumns = `name, region, runner_url, runner_token, ergo_url, ergo_token, chat_url, chat_key,
	status, problem, owner, owner_nick, template, timezone, anthropic_key, updated_at, model_keys`

func (s *Postgres) networkValues(n Network) ([]any, error) {
	sec, err := s.seal(n)
	if err != nil {
		return nil, err
	}
	status := n.Status
	if status == "" {
		status = Ready
	}
	var owner *string
	if n.Owner != "" {
		owner = &n.Owner
	}
	updated := n.Updated
	if updated.IsZero() {
		updated = time.Now()
	}
	// anthropic_key is from before the providers' keys were kept together
	// (model_keys): the upgrade moved what was there, and it's never written.
	return []any{strings.ToLower(n.Name), n.Region, n.RunnerURL, sec.runnerToken, n.ErgoURL, sec.ergoToken, n.ChatURL, sec.chatKey,
		status, n.Problem, owner, n.OwnerNick, n.Template, n.Timezone, nil, updated, sec.modelKeys}, nil
}

func (s *Postgres) AddNetwork(ctx context.Context, n Network) error {
	vals, err := s.networkValues(n)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into networks (`+networkColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`, vals...)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrTaken
	}
	return err
}

// PutNetwork adds a network or replaces its details, for bootstrapping the
// local network from the environment.
func (s *Postgres) PutNetwork(ctx context.Context, n Network) error {
	vals, err := s.networkValues(n)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into networks (`+networkColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		on conflict (name) do update set region = $2, runner_url = $3, runner_token = $4, ergo_url = $5, ergo_token = $6,
			chat_url = $7, chat_key = $8, status = $9, problem = $10, owner = $11, owner_nick = $12, template = $13,
			timezone = $14, anthropic_key = $15, updated_at = $16, model_keys = $17`, vals...)
	return err
}

func (s *Postgres) UpdateNetwork(ctx context.Context, n Network) error {
	vals, err := s.networkValues(n)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `update networks set region = $2, runner_url = $3, runner_token = $4, ergo_url = $5, ergo_token = $6,
		chat_url = $7, chat_key = $8, status = $9, problem = $10, owner = $11, owner_nick = $12, template = $13,
		timezone = $14, anthropic_key = $15, updated_at = $16, model_keys = $17 where name = $1`, vals...)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// RemoveNetwork forgets a network; its sign-ins go with it.
func (s *Postgres) RemoveNetwork(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `delete from networks where name = $1`, strings.ToLower(name))
	return err
}

func (s *Postgres) Network(ctx context.Context, name string) (Network, error) {
	n, err := s.scanNetwork(s.pool.QueryRow(ctx, `select `+networkColumns+` from networks where name = $1`, strings.ToLower(name)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Network{}, ErrNotFound
	}
	return n, err
}

func (s *Postgres) NetworksOwnedBy(ctx context.Context, person string) ([]Network, error) {
	rows, err := s.pool.Query(ctx, `select `+networkColumns+` from networks where owner = $1 order by name`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Network
	for rows.Next() {
		n, err := s.scanNetwork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Postgres) scanNetwork(row pgx.Row) (Network, error) {
	var n Network
	var sec sealed
	var owner *string
	var anthropicKey []byte // not read: from before model_keys (the upgrade moved it)
	err := row.Scan(&n.Name, &n.Region, &n.RunnerURL, &sec.runnerToken, &n.ErgoURL, &sec.ergoToken, &n.ChatURL, &sec.chatKey,
		&n.Status, &n.Problem, &owner, &n.OwnerNick, &n.Template, &n.Timezone, &anthropicKey, &n.Updated, &sec.modelKeys)
	if err != nil {
		return Network{}, err
	}
	if owner != nil {
		n.Owner = *owner
	}
	n.Updated = n.Updated.UTC()
	runner, ergo, chat, sealedKeys := networkSecrets(n.Name, n.RunnerURL, n.ErgoURL, n.ChatURL)
	var keys string
	for _, f := range []struct {
		sealed []byte
		aad    []byte
		into   *string
	}{{sec.runnerToken, runner, &n.RunnerToken}, {sec.ergoToken, ergo, &n.ErgoToken}, {sec.chatKey, chat, &n.ChatKey}, {sec.modelKeys, sealedKeys, &keys}} {
		if f.sealed == nil {
			continue
		}
		plain, err := s.sealer.Open(f.sealed, f.aad)
		if err != nil {
			return Network{}, fmt.Errorf("network %s: %w", n.Name, err)
		}
		*f.into = string(plain)
	}
	if keys != "" {
		var byProvider map[models.Provider]string
		if err := json.Unmarshal([]byte(keys), &byProvider); err != nil {
			return Network{}, fmt.Errorf("network %s's model keys: %w", n.Name, err)
		}
		n.Keys = ModelKeys{}
		for p, k := range byProvider {
			n.Keys.Set(p, k)
		}
	}
	return n, nil
}

// keysJSON is a network's model keys as sealed together, "" for none.
func keysJSON(k ModelKeys) string {
	m := k.Map()
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (s *Postgres) GooglePerson(ctx context.Context, subject, email string) (Person, error) {
	var p Person
	email = strings.ToLower(email)
	err := s.pool.QueryRow(ctx, `update people set email = $2 where google_sub = $1 returning id, email, blocked`, subject, email).
		Scan(&p.ID, &p.Email, &p.Blocked)
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	// Someone who signed in by email with this address, and has no Google
	// account yet, is this one.
	err = s.pool.QueryRow(ctx, `update people set google_sub = $1 where id = (
			select id from people where email = $2 and google_sub is null order by created_at, id limit 1)
		returning id, email, blocked`, subject, email).Scan(&p.ID, &p.Email, &p.Blocked)
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	err = s.pool.QueryRow(ctx, `insert into people (id, google_sub, email) values ($1, $2, $3)
		on conflict (google_sub) do update set email = $3 returning id, email, blocked`, newID(), subject, email).Scan(&p.ID, &p.Email, &p.Blocked)
	return p, err
}

func (s *Postgres) EmailPerson(ctx context.Context, email string) (Person, error) {
	var p Person
	email = strings.ToLower(strings.TrimSpace(email))
	err := s.pool.QueryRow(ctx, `select id, email, blocked from people where email = $1 order by created_at, id limit 1`, email).
		Scan(&p.ID, &p.Email, &p.Blocked)
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	err = s.pool.QueryRow(ctx, `insert into people (id, email) values ($1, $2) returning id, email, blocked`, newID(), email).
		Scan(&p.ID, &p.Email, &p.Blocked)
	return p, err
}

func (s *Postgres) Person(ctx context.Context, id string) (Person, error) {
	var p Person
	err := s.pool.QueryRow(ctx, `select id, email, blocked from people where id = $1`, id).Scan(&p.ID, &p.Email, &p.Blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, ErrNotFound
	}
	return p, err
}

func (s *Postgres) Everyone(ctx context.Context) ([]PersonRecord, error) {
	rows, err := s.pool.Query(ctx, `select id, email, blocked, created_at, last_seen from people order by created_at desc, email`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PersonRecord, error) {
		var p PersonRecord
		var seen *time.Time
		err := row.Scan(&p.ID, &p.Email, &p.Blocked, &p.Joined, &seen)
		p.Joined = p.Joined.UTC()
		if seen != nil {
			p.LastSeen = seen.UTC()
		}
		return p, err
	})
}

func (s *Postgres) SetBlocked(ctx context.Context, id string, blocked bool) error {
	tag, err := s.pool.Exec(ctx, `update people set blocked = $2 where id = $1`, id, blocked)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) Seen(ctx context.Context, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update people set last_seen = $2 where id = $1`, id, at)
	return err
}

func (s *Postgres) AllNetworks(ctx context.Context) ([]Network, error) {
	rows, err := s.pool.Query(ctx, `select `+networkColumns+` from networks order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Network
	for rows.Next() {
		n, err := s.scanNetwork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Postgres) LogAdmin(ctx context.Context, e AdminEntry) error {
	_, err := s.pool.Exec(ctx, `insert into admin_log (at, admin, action, target) values ($1, $2, $3, $4)`, e.At, e.Admin, e.Action, e.Target)
	return err
}

func (s *Postgres) AdminLog(ctx context.Context, limit int) ([]AdminEntry, error) {
	rows, err := s.pool.Query(ctx, `select at, admin, action, target from admin_log order by id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminEntry, error) {
		var e AdminEntry
		err := row.Scan(&e.At, &e.Admin, &e.Action, &e.Target)
		e.At = e.At.UTC()
		return e, err
	})
}

const linkColumns = `id, network, kind, account, role, team, guest, created_by, created_at, expires_at, used_at`

func scanLink(row pgx.Row) (Link, error) {
	var l Link
	var used *time.Time
	err := row.Scan(&l.ID, &l.Network, &l.Kind, &l.Account, &l.Role, &l.Team, &l.Guest, &l.CreatedBy, &l.Created, &l.Expires, &used)
	l.Created, l.Expires = l.Created.UTC(), l.Expires.UTC()
	if used != nil {
		l.Used = used.UTC()
	}
	return l, err
}

func (s *Postgres) AddLink(ctx context.Context, hash string, l Link) error {
	_, err := s.pool.Exec(ctx, `insert into links (hash, `+linkColumns+`) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, null)`,
		s.sealer.Hash(hash), l.ID, strings.ToLower(l.Network), l.Kind, l.Account, l.Role, l.Team, l.Guest, l.CreatedBy, l.Created, l.Expires)
	return err
}

func (s *Postgres) Link(ctx context.Context, hash string) (Link, error) {
	l, err := scanLink(s.pool.QueryRow(ctx, `select `+linkColumns+` from links where hash = any($1)`, s.sealer.Hashes(hash)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

func (s *Postgres) UseLink(ctx context.Context, hash, account string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `update links set used_at = $3, account = $2 where hash = any($1) and used_at is null`, s.sealer.Hashes(hash), account, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Link(ctx, hash); err != nil {
			return err
		}
		return ErrLinkUsed
	}
	return nil
}

func (s *Postgres) ReleaseLink(ctx context.Context, hash string) error {
	_, err := s.pool.Exec(ctx, `update links set used_at = null, account = case when kind = 'invite' then '' else account end where hash = any($1)`, s.sealer.Hashes(hash))
	return err
}

func (s *Postgres) PendingLinks(ctx context.Context, network, kind string, now time.Time) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `select `+linkColumns+` from links
		where network = $1 and kind = $2 and used_at is null and expires_at > $3 order by created_at desc`, strings.ToLower(network), kind, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Link, error) { return scanLink(row) })
}

func (s *Postgres) CancelLink(ctx context.Context, network, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `update links set used_at = coalesce(used_at, $3) where network = $1 and id = $2`, strings.ToLower(network), id, at)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) DeleteAccount(ctx context.Context, network, account, keep string) error {
	_, err := s.pool.Exec(ctx, `delete from sessions where coalesce(network, '') = $1 and lower(account) = lower($2) and not (hash = any($3))`, strings.ToLower(network), account, s.sealer.Hashes(keep))
	return err
}

func (s *Postgres) OnboardingDone(ctx context.Context, network string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `select step from onboarding where network = $1`, strings.ToLower(network))
	if err != nil {
		return nil, err
	}
	steps, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := map[string]bool{}
	for _, st := range steps {
		out[st] = true
	}
	return out, err
}

func (s *Postgres) MarkOnboarding(ctx context.Context, network, step string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `insert into onboarding (network, step, done_at) values ($1, $2, $3) on conflict do nothing`,
		strings.ToLower(network), step, at)
	return err
}

func (s *Postgres) Save(ctx context.Context, hash string, sess Session) error {
	_, err := s.pool.Exec(ctx, `insert into sessions (hash, network, account, csrf, expires) values ($1, nullif($2, ''), $3, $4, $5)`,
		s.sealer.Hash(hash), strings.ToLower(sess.Network), sess.Account, sess.CSRF, sess.Expires)
	return err
}

func (s *Postgres) Get(ctx context.Context, hash string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `select coalesce(network, ''), account, csrf, expires from sessions where hash = any($1)`, s.sealer.Hashes(hash)).
		Scan(&sess.Network, &sess.Account, &sess.CSRF, &sess.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	sess.Expires = sess.Expires.UTC()
	return sess, err
}

func (s *Postgres) Delete(ctx context.Context, hash string) error {
	_, err := s.pool.Exec(ctx, `delete from sessions where hash = any($1)`, s.sealer.Hashes(hash))
	return err
}

func (s *Postgres) DeleteExpired(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `delete from sessions where expires <= $1`, now)
	return err
}

func (s *Postgres) GitHubOf(ctx context.Context, network string) (GitHubLink, error) {
	var l GitHubLink
	err := s.pool.QueryRow(ctx, `select installation, account, connected_by, connected_at from network_github where network = $1`,
		strings.ToLower(network)).Scan(&l.Installation, &l.Account, &l.By, &l.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubLink{}, nil
	}
	return l, err
}

func (s *Postgres) BindGitHub(ctx context.Context, network string, l GitHubLink) error {
	_, err := s.pool.Exec(ctx, `insert into network_github (network, installation, account, connected_by, connected_at)
		values ($1, $2, $3, $4, $5)
		on conflict (network) do update set installation = $2, account = $3, connected_by = $4, connected_at = $5`,
		strings.ToLower(network), l.Installation, l.Account, l.By, l.At)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrGitHubTaken
	}
	return err
}

func (s *Postgres) UnbindGitHub(ctx context.Context, network string) error {
	_, err := s.pool.Exec(ctx, `delete from network_github where network = $1`, strings.ToLower(network))
	return err
}
