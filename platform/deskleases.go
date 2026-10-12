// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeskLeases keeps the desks at work (brokerDeskStart's leases) where a
// restart of the console doesn't forget them: a desk that started before a
// deploy says how long it worked after it, and holds its place among the
// plan's desks at once meanwhile. A store that isn't one keeps them in the
// console's memory.
type DeskLeases interface {
	StartDeskLease(ctx context.Context, id string, l DeskLease) error
	// EndDeskLease forgets network's lease id and says what it was, or
	// ErrNotFound.
	EndDeskLease(ctx context.Context, id, network string) (DeskLease, error)
	// DesksAtWork is how many of network's leases started since then.
	DesksAtWork(ctx context.Context, network string, since time.Time) (int, error)
	// ForgetDeskLeases forgets leases that started before then.
	ForgetDeskLeases(ctx context.Context, before time.Time) error
}

// DeskLease is one of a network's desks at work: whose, and since when.
type DeskLease struct {
	Network, Agent string
	Started        time.Time
}

// leases is where the console keeps its desks at work.
func (p *Platform) leases() DeskLeases {
	if l, ok := p.opts.Networks.(DeskLeases); ok {
		return l
	}
	return &p.desks
}

// The console's memory's (for a store that keeps none), and the memory
// store's.

type leaseMap map[string]DeskLease

func (m *leaseMap) start(id string, l DeskLease) {
	if *m == nil {
		*m = leaseMap{}
	}
	l.Network = strings.ToLower(l.Network)
	(*m)[id] = l
}

func (m leaseMap) end(id, network string) (DeskLease, error) {
	l, ok := m[id]
	if !ok || l.Network != strings.ToLower(network) {
		return DeskLease{}, ErrNotFound
	}
	delete(m, id)
	return l, nil
}

func (m leaseMap) working(network string, since time.Time) int {
	n := 0
	for _, l := range m {
		if l.Network == strings.ToLower(network) && l.Started.After(since) {
			n++
		}
	}
	return n
}

func (m leaseMap) forget(before time.Time) {
	for id, l := range m {
		if l.Started.Before(before) {
			delete(m, id)
		}
	}
}

func (d *desksAtWork) StartDeskLease(_ context.Context, id string, l DeskLease) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.leases.start(id, l)
	return nil
}

func (d *desksAtWork) EndDeskLease(_ context.Context, id, network string) (DeskLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leases.end(id, network)
}

func (d *desksAtWork) DesksAtWork(_ context.Context, network string, since time.Time) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leases.working(network, since), nil
}

func (d *desksAtWork) ForgetDeskLeases(_ context.Context, before time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.leases.forget(before)
	return nil
}

func (s *MemoryStore) StartDeskLease(_ context.Context, id string, l DeskLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deskLeases.start(id, l)
	return nil
}

func (s *MemoryStore) EndDeskLease(_ context.Context, id, network string) (DeskLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deskLeases.end(id, network)
}

func (s *MemoryStore) DesksAtWork(_ context.Context, network string, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deskLeases.working(network, since), nil
}

func (s *MemoryStore) ForgetDeskLeases(_ context.Context, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deskLeases.forget(before)
	return nil
}

// Postgres's.

func (s *Postgres) StartDeskLease(ctx context.Context, id string, l DeskLease) error {
	_, err := s.pool.Exec(ctx, `insert into desk_leases (id, network, agent, started) values ($1, $2, $3, $4)`,
		id, strings.ToLower(l.Network), l.Agent, l.Started)
	return err
}

func (s *Postgres) EndDeskLease(ctx context.Context, id, network string) (DeskLease, error) {
	var l DeskLease
	err := s.pool.QueryRow(ctx, `delete from desk_leases where id = $1 and network = $2 returning network, agent, started`,
		id, strings.ToLower(network)).Scan(&l.Network, &l.Agent, &l.Started)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (s *Postgres) DesksAtWork(ctx context.Context, network string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from desk_leases where network = $1 and started > $2`,
		strings.ToLower(network), since).Scan(&n)
	return n, err
}

func (s *Postgres) ForgetDeskLeases(ctx context.Context, before time.Time) error {
	_, err := s.pool.Exec(ctx, `delete from desk_leases where started < $1`, before)
	return err
}
