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
	// BeatDeskLease is network's lease id still working at at, or
	// ErrNotFound.
	BeatDeskLease(ctx context.Context, id, network string, at time.Time) error
	// EndDeskLease forgets network's lease id and says what it was, or
	// ErrNotFound.
	EndDeskLease(ctx context.Context, id, network string) (DeskLease, error)
	// DesksAtWork is how many of network's leases were heard from since
	// then.
	DesksAtWork(ctx context.Context, network string, since time.Time) (int, error)
	// TakeLostDeskLeases forgets the leases last heard from before then,
	// and says what they were.
	TakeLostDeskLeases(ctx context.Context, before time.Time) ([]DeskLease, error)
}

// DeskLease is one of a network's desks at work: whose, since when, and
// when its runner last said it was working (Beat).
type DeskLease struct {
	Network, Agent string
	Started, Beat  time.Time
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
	if l.Beat.IsZero() {
		l.Beat = l.Started
	}
	(*m)[id] = l
}

func (m leaseMap) beat(id, network string, at time.Time) error {
	l, ok := m[id]
	if !ok || l.Network != strings.ToLower(network) {
		return ErrNotFound
	}
	l.Beat = at
	m[id] = l
	return nil
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
		if l.Network == strings.ToLower(network) && l.Beat.After(since) {
			n++
		}
	}
	return n
}

func (m leaseMap) takeLost(before time.Time) []DeskLease {
	var lost []DeskLease
	for id, l := range m {
		if l.Beat.Before(before) {
			lost = append(lost, l)
			delete(m, id)
		}
	}
	return lost
}

func (d *desksAtWork) StartDeskLease(_ context.Context, id string, l DeskLease) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.leases.start(id, l)
	return nil
}

func (d *desksAtWork) BeatDeskLease(_ context.Context, id, network string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leases.beat(id, network, at)
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

func (d *desksAtWork) TakeLostDeskLeases(_ context.Context, before time.Time) ([]DeskLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leases.takeLost(before), nil
}

func (s *MemoryStore) StartDeskLease(_ context.Context, id string, l DeskLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deskLeases.start(id, l)
	return nil
}

func (s *MemoryStore) BeatDeskLease(_ context.Context, id, network string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deskLeases.beat(id, network, at)
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

func (s *MemoryStore) TakeLostDeskLeases(_ context.Context, before time.Time) ([]DeskLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deskLeases.takeLost(before), nil
}

// Postgres's.

func (s *Postgres) StartDeskLease(ctx context.Context, id string, l DeskLease) error {
	_, err := s.pool.Exec(ctx, `insert into desk_leases (id, network, agent, started, beat) values ($1, $2, $3, $4, $4)`,
		id, strings.ToLower(l.Network), l.Agent, l.Started)
	return err
}

func (s *Postgres) BeatDeskLease(ctx context.Context, id, network string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `update desk_leases set beat = greatest(beat, $3) where id = $1 and network = $2`,
		id, strings.ToLower(network), at)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) EndDeskLease(ctx context.Context, id, network string) (DeskLease, error) {
	var l DeskLease
	err := s.pool.QueryRow(ctx, `delete from desk_leases where id = $1 and network = $2 returning network, agent, started, beat`,
		id, strings.ToLower(network)).Scan(&l.Network, &l.Agent, &l.Started, &l.Beat)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (s *Postgres) DesksAtWork(ctx context.Context, network string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from desk_leases where network = $1 and beat > $2`,
		strings.ToLower(network), since).Scan(&n)
	return n, err
}

func (s *Postgres) TakeLostDeskLeases(ctx context.Context, before time.Time) ([]DeskLease, error) {
	rows, err := s.pool.Query(ctx, `delete from desk_leases where beat < $1 returning network, agent, started, beat`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeskLease, error) {
		var l DeskLease
		err := r.Scan(&l.Network, &l.Agent, &l.Started, &l.Beat)
		return l, err
	})
}
