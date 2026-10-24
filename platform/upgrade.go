// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/internal/connectors"
)

// sealV1 is the upgrade that seals every secret again bound to where it's
// kept (aad), with the key's ID in front, and keys the hashes tokens are
// kept by: before it, a sealed value moved to another row or network still
// opened, and anyone who could write to the database could put in a
// session or a link of their own. Values sealed the old way are opened only
// here; after it, they aren't taken. Hashes are keyed in place (an HMAC of
// the hash that was there), so nobody is signed out, and links and
// recovery codes still work.
const sealV1 = "seal v1"

// legacyOpener opens values sealed before v1.
type legacyOpener interface {
	openLegacy(sealed []byte) ([]byte, error)
}

// upgrade makes what upgrades haven't been made yet, each in one go.
func (s *Postgres) upgrade(ctx context.Context) error {
	var done bool
	if err := s.pool.QueryRow(ctx, `select exists (select 1 from upgrades where name = $1)`, sealV1).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	legacy, ok := s.sealer.(legacyOpener)
	if !ok {
		return errors.New("this sealer can't open secrets sealed before v1, so it can't seal them again")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, step := range []func(context.Context, pgx.Tx, legacyOpener) error{
		s.resealNetworks, s.resealConnections, s.resealOAuthClients, s.resealTwoSteps, s.keyHashes,
	} {
		if err := step(ctx, tx, legacy); err != nil {
			return fmt.Errorf("sealing again (%s): %w", sealV1, err)
		}
	}
	if _, err := tx.Exec(ctx, `insert into upgrades (name) values ($1)`, sealV1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// reseal opens old (sealed before v1) and seals it again bound to aad; nil
// stays nil.
func (s *Postgres) reseal(legacy legacyOpener, old, aad []byte) ([]byte, error) {
	if old == nil {
		return nil, nil
	}
	plain, err := legacy.openLegacy(old)
	if err != nil {
		return nil, err
	}
	return s.sealer.Seal(plain, aad)
}

func (s *Postgres) resealNetworks(ctx context.Context, tx pgx.Tx, legacy legacyOpener) error {
	type row struct {
		name, runnerURL, ergoURL, chatURL   string
		runner, ergo, chat, anthropic, keys []byte
	}
	rows, err := tx.Query(ctx, `select name, runner_url, ergo_url, chat_url, runner_token, ergo_token, chat_key, anthropic_key, model_keys from networks`)
	if err != nil {
		return err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var n row
		err := r.Scan(&n.name, &n.runnerURL, &n.ergoURL, &n.chatURL, &n.runner, &n.ergo, &n.chat, &n.anthropic, &n.keys)
		return n, err
	})
	if err != nil {
		return err
	}
	for _, n := range all {
		runnerAAD, ergoAAD, chatAAD, keysAAD := networkSecrets(n.name, n.runnerURL, n.ergoURL, n.chatURL)
		runner, err1 := s.reseal(legacy, n.runner, runnerAAD)
		ergo, err2 := s.reseal(legacy, n.ergo, ergoAAD)
		chat, err3 := s.reseal(legacy, n.chat, chatAAD)
		if err := errors.Join(err1, err2, err3); err != nil {
			return fmt.Errorf("network %s: %w", n.name, err)
		}
		// A network from before the providers' keys were kept together has
		// only its Anthropic key: it goes with them now.
		keys := n.keys
		if keys == nil && n.anthropic != nil {
			key, err := legacy.openLegacy(n.anthropic)
			if err != nil {
				return fmt.Errorf("network %s's Anthropic key: %w", n.name, err)
			}
			b, _ := json.Marshal(map[string]string{"anthropic": string(key)})
			if keys, err = s.sealer.Seal(b, keysAAD); err != nil {
				return err
			}
		} else if keys, err = s.reseal(legacy, keys, keysAAD); err != nil {
			return fmt.Errorf("network %s's model keys: %w", n.name, err)
		}
		if _, err := tx.Exec(ctx, `update networks set runner_token = $2, ergo_token = $3, chat_key = $4, model_keys = $5, anthropic_key = null where name = $1`,
			n.name, runner, ergo, chat, keys); err != nil {
			return err
		}
	}
	return nil
}

func (s *Postgres) resealConnections(ctx context.Context, tx pgx.Tx, legacy legacyOpener) error {
	type row struct {
		network, connector, clientID, method string
		server                               []byte
		secret, access, refresh              []byte
	}
	rows, err := tx.Query(ctx, `select network, connector, server, client_id, auth_method, client_secret, access_token, refresh_token from connections`)
	if err != nil {
		return err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var c row
		err := r.Scan(&c.network, &c.connector, &c.server, &c.clientID, &c.method, &c.secret, &c.access, &c.refresh)
		return c, err
	})
	if err != nil {
		return err
	}
	for _, c := range all {
		var server connectors.Server
		if err := json.Unmarshal(c.server, &server); err != nil {
			return err
		}
		secretAAD, accessAAD, refreshAAD, err := connectionSecrets(c.network, c.connector, server, connectors.Client{ID: c.clientID, Method: c.method}, nil) // from before connectors of networks' own
		if err != nil {
			return err
		}
		secret, err1 := s.reseal(legacy, c.secret, secretAAD)
		access, err2 := s.reseal(legacy, c.access, accessAAD)
		refresh, err3 := s.reseal(legacy, c.refresh, refreshAAD)
		if err := errors.Join(err1, err2, err3); err != nil {
			return fmt.Errorf("%s's %s connection: %w", c.network, c.connector, err)
		}
		if _, err := tx.Exec(ctx, `update connections set client_secret = $3, access_token = $4, refresh_token = $5 where network = $1 and connector = $2`,
			c.network, c.connector, secret, access, refresh); err != nil {
			return err
		}
	}
	return nil
}

func (s *Postgres) resealOAuthClients(ctx context.Context, tx pgx.Tx, legacy legacyOpener) error {
	type row struct {
		issuer, redirect, id, method string
		secret                       []byte
	}
	rows, err := tx.Query(ctx, `select issuer, redirect_uri, client_id, auth_method, client_secret from oauth_clients`)
	if err != nil {
		return err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var c row
		err := r.Scan(&c.issuer, &c.redirect, &c.id, &c.method, &c.secret)
		return c, err
	})
	if err != nil {
		return err
	}
	for _, c := range all {
		secret, err := s.reseal(legacy, c.secret, oauthClientSecret(c.issuer, c.redirect, connectors.Client{ID: c.id, Method: c.method}))
		if err != nil {
			return fmt.Errorf("%s's client: %w", c.issuer, err)
		}
		if _, err := tx.Exec(ctx, `update oauth_clients set client_secret = $3 where issuer = $1 and redirect_uri = $2`, c.issuer, c.redirect, secret); err != nil {
			return err
		}
	}
	return nil
}

func (s *Postgres) resealTwoSteps(ctx context.Context, tx pgx.Tx, legacy legacyOpener) error {
	type row struct {
		network, account string
		secret           []byte
		recovery         []string
	}
	rows, err := tx.Query(ctx, `select network, account, secret, recovery from two_step`)
	if err != nil {
		return err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var t row
		err := r.Scan(&t.network, &t.account, &t.secret, &t.recovery)
		return t, err
	})
	if err != nil {
		return err
	}
	for _, t := range all {
		secret, err := s.reseal(legacy, t.secret, twoStepSecret(t.network, t.account))
		if err != nil {
			return fmt.Errorf("%s's two-step on %s: %w", t.account, t.network, err)
		}
		if _, err := tx.Exec(ctx, `update two_step set secret = $3, recovery = $4 where network = $1 and account = $2`,
			t.network, t.account, secret, s.keyed(t.recovery)); err != nil {
			return err
		}
	}
	homeRows, err := tx.Query(ctx, `select person, secret, recovery from home_two_step`)
	if err != nil {
		return err
	}
	home, err := pgx.CollectRows(homeRows, func(r pgx.CollectableRow) (row, error) {
		var t row
		err := r.Scan(&t.account, &t.secret, &t.recovery)
		return t, err
	})
	if err != nil {
		return err
	}
	for _, t := range home {
		secret, err := s.reseal(legacy, t.secret, homeTwoStepSecret(t.account))
		if err != nil {
			return fmt.Errorf("%s's home two-step: %w", t.account, err)
		}
		if _, err := tx.Exec(ctx, `update home_two_step set secret = $2, recovery = $3 where person = $1`, t.account, secret, s.keyed(t.recovery)); err != nil {
			return err
		}
	}
	return nil
}

// keyHashes keys the hashes sessions, links and email confirmations are
// kept by, in place: whoever holds a token still finds it.
func (s *Postgres) keyHashes(ctx context.Context, tx pgx.Tx, _ legacyOpener) error {
	for _, t := range []struct{ table, column string }{{"sessions", "hash"}, {"links", "hash"}, {"alerts", "confirm_hash"}} {
		rows, err := tx.Query(ctx, `select `+t.column+` from `+t.table+` where `+t.column+` is not null`)
		if err != nil {
			return err
		}
		hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.Exec(ctx, `update `+t.table+` set `+t.column+` = $2 where `+t.column+` = $1`, h, s.sealer.Hash(h)); err != nil {
				return err
			}
		}
	}
	return nil
}
