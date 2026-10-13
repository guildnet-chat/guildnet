// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The open console has no plans or desk hours: GuildNet's hosted edition
// adds them through an extension (platform.Extension). Its desk policy says
// whether a network's desk may start, how many may work at once, and counts
// the time worked; the broker keeps the leases and never lets a desk count
// more time than has passed.

// fakeDesks is a desk policy a test controls.
type fakeDesks struct {
	mu      sync.Mutex
	refused string
	atOnce  int
	storage int
	err     error
	worked  []worked
}

type worked struct {
	network, agent string
	seconds        int64
}

func (f *fakeDesks) Limits(_ context.Context, n platform.Network, agent string) (platform.DeskLimits, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return platform.DeskLimits{Refused: f.refused, AtOnce: f.atOnce, StorageGB: f.storage, Plan: "house"}, f.err
}

func (f *fakeDesks) Worked(_ context.Context, n platform.Network, agent string, seconds int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.worked = append(f.worked, worked{n.Name, agent, seconds})
	return nil
}

func withDeskPolicy(d platform.DeskPolicy) func(*Setup) {
	return func(o *Setup) {
		// A host of many networks, like GuildNet's own.
		o.Extension = platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
			return platform.Hooks{Desks: d, MayCreate: anyone}
		})
	}
}

// anyone may create a network, as many as they like.
func anyone(context.Context, platform.Person, int) (platform.CreateAnswer, error) {
	return platform.CreateAnswer{}, nil
}

func deskPolicyWorld(t *testing.T, d platform.DeskPolicy) *World {
	t.Helper()
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey}
	}, withDeskPolicy(d))
	w.Created("ada@example.com", "initech")
	w.Created("bob@example.com", "hooli")
	return w
}

func TestAnExtensionsDeskPolicyDecidesWhetherADeskStarts(t *testing.T) {
	d := &fakeDesks{refused: "the house says no"}
	w := deskPolicyWorld(t, d)

	_, status, out := StartDesk(t, w, "initech", "kai", false)
	if status != http.StatusPaymentRequired || out["reason"] != "the house says no" {
		t.Errorf("refused: %d %v, want 402 with the policy's reason", status, out)
	}

	// Work under way (after an approval) isn't refused.
	id, status, out := StartDesk(t, w, "initech", "kai", true)
	if status != http.StatusOK || id == "" || out["plan"] != "house" {
		t.Errorf("resuming: %d %v, want a lease on the policy's plan", status, out)
	}
}

func TestAnExtensionsDeskPolicySaysHowManyWorkAtOnce(t *testing.T) {
	d := &fakeDesks{atOnce: 1}
	w := deskPolicyWorld(t, d)

	first, status, _ := StartDesk(t, w, "initech", "kai", false)
	if status != http.StatusOK {
		t.Fatalf("first desk: %d", status)
	}
	if _, status, out := StartDesk(t, w, "initech", "pax", false); status != http.StatusTooManyRequests || out["at_once"] != float64(1) {
		t.Errorf("second desk: %d %v, want 429 at one at once", status, out)
	}
	if _, status, _ := StartDesk(t, w, "hooli", "pax", false); status != http.StatusOK {
		t.Errorf("another network's desk: %d, want it to start", status)
	}
	DeskCall(t, w, "initech", RunnerSecret(w, "initech"), "/desks/done", map[string]any{"id": first, "seconds": 1})
	if _, status, _ := StartDesk(t, w, "initech", "pax", false); status != http.StatusOK {
		t.Errorf("after the first finished: %d, want the second to start", status)
	}
}

func TestADeskPolicyWithNoLimitLetsEveryDeskWork(t *testing.T) {
	w := deskPolicyWorld(t, &fakeDesks{})

	for _, agent := range []string{"kai", "pax", "vera", "dana"} {
		if _, status, out := StartDesk(t, w, "initech", agent, false); status != http.StatusOK {
			t.Errorf("%s's desk: %d %v", agent, status, out)
		}
	}
}

func TestADeskPolicyIsToldTheTimeWorkedNeverMoreThanPassed(t *testing.T) {
	d := &fakeDesks{}
	w := deskPolicyWorld(t, d)

	id, _, _ := StartDesk(t, w, "initech", "kai", false)
	w.Clock.Advance(90 * time.Second)
	if status, out := DeskCall(t, w, "initech", RunnerSecret(w, "initech"), "/desks/done", map[string]any{"id": id, "seconds": 10 * 3600}); status != http.StatusOK {
		t.Fatalf("done: %d %v", status, out)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.worked) != 1 || d.worked[0] != (worked{"initech", "kai", 90}) {
		t.Errorf("worked = %+v, want initech's kai for 90 seconds", d.worked)
	}
}

func TestADeskPolicyThatFailsStopsTheDeskSafely(t *testing.T) {
	w := deskPolicyWorld(t, &fakeDesks{err: errors.New("the ledger is down")})

	if _, status, out := StartDesk(t, w, "initech", "kai", false); status != http.StatusInternalServerError {
		t.Errorf("start: %d %v, want 500", status, out)
	}
}

// Without an extension there's no desk policy: the broker doesn't count desk
// time at all, and runners take that as desks being uncounted (as many as
// sandboxd has room for).
func TestWithoutAnExtensionDesksArentCounted(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey}
	})
	w.Created("ada@example.com", "initech")

	if _, status, _ := StartDesk(t, w, "initech", "kai", false); status != http.StatusNotFound {
		t.Errorf("start: %d, want 404 (not counted)", status)
	}
}

// The runner asks through desk.Broker: this is its side of the same calls,
// whatever the policy says.
func TestARunnersBrokerClientGetsItsTurns(t *testing.T) {
	d := &fakeDesks{atOnce: 1}
	w := deskPolicyWorld(t, d)
	srv := httptest.NewServer(w.Broker)
	defer srv.Close()
	b := &desk.Broker{URL: srv.URL, Network: "initech", Secret: RunnerSecret(w, "initech")}
	ctx := context.Background()

	turn, err := b.StartDesk(ctx, "kai", false)
	if err != nil || turn.ID == "" || turn.Busy || turn.Refused != "" {
		t.Fatalf("the first desk: %+v %v", turn, err)
	}
	if busy, err := b.StartDesk(ctx, "vera", false); err != nil || !busy.Busy {
		t.Errorf("a second desk, one at once: %+v %v", busy, err)
	}
	w.Clock.Advance(3 * time.Hour)
	if err := b.DeskDone(ctx, turn.ID, 3*3600); err != nil {
		t.Errorf("reporting its time: %v", err)
	}
	d.mu.Lock()
	d.refused = "the house says no"
	d.mu.Unlock()
	if out, err := b.StartDesk(ctx, "kai", false); err != nil || out.Refused != "the house says no" {
		t.Errorf("refused: %+v %v", out, err)
	}
	if err := b.DeskDone(ctx, turn.ID, 60); err == nil {
		t.Error("reporting the same desk twice worked")
	}
}
