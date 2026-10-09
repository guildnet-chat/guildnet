// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AlertSettings are where someone in a network wants email alerts, and
// which. The owner's address is their Google account's, so theirs is never
// kept here; only whether they want approvals.
type AlertSettings struct {
	Email     string // confirmed: alerts go here
	Pending   string // given, waiting for its confirmation link
	Approvals bool   // email them when an approval is waiting on them
}

// Alerts keeps people's alert settings, by network and account.
type Alerts interface {
	AlertSettings(ctx context.Context, network, account string) (AlertSettings, error) // zero if none
	// AwaitConfirmation keeps email as account's pending address, confirmed
	// by the link whose secret hashes to hash, until until. A new one
	// replaces any before it.
	AwaitConfirmation(ctx context.Context, network, account, email, hash string, until time.Time) error
	// Confirm makes the pending address behind hash its account's, once.
	// ErrNotFound if there's no such link (or it was used), ErrLinkUsed if
	// it expired.
	Confirm(ctx context.Context, hash string, now time.Time) (network, account, email string, err error)
	SetApprovalAlerts(ctx context.Context, network, account string, on bool) error
}

type alertRow struct {
	AlertSettings
	hash  string
	until time.Time
}

func alertKey(network, account string) string {
	return strings.ToLower(network) + "|" + strings.ToLower(account)
}

func (s *MemoryStore) AlertSettings(_ context.Context, network, account string) (AlertSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerts[alertKey(network, account)].AlertSettings, nil
}

func (s *MemoryStore) AwaitConfirmation(_ context.Context, network, account, email, hash string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alerts == nil {
		s.alerts = map[string]alertRow{}
	}
	row := s.alerts[alertKey(network, account)]
	row.Pending, row.hash, row.until = email, hash, until
	s.alerts[alertKey(network, account)] = row
	return nil
}

func (s *MemoryStore) Confirm(_ context.Context, hash string, now time.Time) (string, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, row := range s.alerts {
		if hash == "" || row.hash != hash {
			continue
		}
		if !now.Before(row.until) {
			return "", "", "", ErrLinkUsed
		}
		row.Email, row.Pending, row.hash = row.Pending, "", ""
		s.alerts[k] = row
		network, account, _ := strings.Cut(k, "|")
		return network, account, row.Email, nil
	}
	return "", "", "", ErrNotFound
}

func (s *MemoryStore) SetApprovalAlerts(_ context.Context, network, account string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alerts == nil {
		s.alerts = map[string]alertRow{}
	}
	row := s.alerts[alertKey(network, account)]
	row.Approvals = on
	s.alerts[alertKey(network, account)] = row
	return nil
}

func (s *Postgres) AlertSettings(ctx context.Context, network, account string) (AlertSettings, error) {
	var a AlertSettings
	err := s.pool.QueryRow(ctx, `select email, pending, approvals from alerts where network = $1 and account = $2`,
		strings.ToLower(network), strings.ToLower(account)).Scan(&a.Email, &a.Pending, &a.Approvals)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertSettings{}, nil
	}
	return a, err
}

func (s *Postgres) AwaitConfirmation(ctx context.Context, network, account, email, hash string, until time.Time) error {
	_, err := s.pool.Exec(ctx, `insert into alerts (network, account, pending, confirm_hash, confirm_until) values ($1, $2, $3, $4, $5)
		on conflict (network, account) do update set pending = $3, confirm_hash = $4, confirm_until = $5`,
		strings.ToLower(network), strings.ToLower(account), email, s.sealer.Hash(hash), until)
	return err
}

func (s *Postgres) Confirm(ctx context.Context, hash string, now time.Time) (string, string, string, error) {
	var network, account, email string
	var until time.Time
	err := s.pool.QueryRow(ctx, `select network, account, pending, confirm_until from alerts where confirm_hash = any($1)`, s.sealer.Hashes(hash)).
		Scan(&network, &account, &email, &until)
	if errors.Is(err, pgx.ErrNoRows) || hash == "" {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	if !now.Before(until) {
		return "", "", "", ErrLinkUsed
	}
	tag, err := s.pool.Exec(ctx, `update alerts set email = pending, pending = '', confirm_hash = null, confirm_until = null
		where confirm_hash = any($1)`, s.sealer.Hashes(hash))
	if err != nil {
		return "", "", "", err
	}
	if tag.RowsAffected() == 0 {
		return "", "", "", ErrNotFound // used meanwhile
	}
	return network, account, email, nil
}

func (s *Postgres) SetApprovalAlerts(ctx context.Context, network, account string, on bool) error {
	_, err := s.pool.Exec(ctx, `insert into alerts (network, account, approvals) values ($1, $2, $3)
		on conflict (network, account) do update set approvals = $3`, strings.ToLower(network), strings.ToLower(account), on)
	return err
}
