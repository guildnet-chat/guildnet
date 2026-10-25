// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network nobody has used for a while can be put to sleep (the hosted
// edition does, for free networks): its IRC server and runner stop, and
// everything is kept. Whoever opens its console next wakes it, in the few
// seconds that takes. The console notes when people (not agents) last used
// each network: in its console, web chat, or an IRC app.

func usedAt(t *testing.T, w *World, network string) time.Time {
	t.Helper()
	n, err := w.Store.Network(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	return n.Used
}

func TestAnAsleepNetworkWakesWhenSomeoneVisits(t *testing.T) {
	h := &house{}
	w := houseWorld(t, h)
	_, console := w.Created("ada@example.com", "initech")
	n, _ := w.Store.Network(context.Background(), "initech")

	if err := h.core.Sleep(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.Store.Network(context.Background(), "initech"); n.Status != platform.Asleep || !w.Host.IsStopped("initech") {
		t.Fatalf("initech asleep: %s, stopped %v", n.Status, w.Host.IsStopped("initech"))
	}
	if r := w.Browser("initech").Get("/agents/v1/inbox"); r.Status == http.StatusOK {
		t.Error("an outside agent reached a sleeping network")
	}

	page := console.Get("/")

	if page.Status != http.StatusServiceUnavailable || !strings.Contains(page.Text(), "Waking initech up") || page.Header.Get("Retry-After") == "" {
		t.Fatalf("visiting it asleep: %d %q\n%s", page.Status, page.Header.Get("Retry-After"), page.Text())
	}
	if page := console.Eventually("/", "Org chart"); page.Status != http.StatusOK {
		t.Fatalf("once awake: %d", page.Status)
	}
	if n, _ := w.Store.Network(context.Background(), "initech"); n.Status != platform.Ready || w.Host.IsStopped("initech") {
		t.Errorf("after the visit: %s, stopped %v", n.Status, w.Host.IsStopped("initech"))
	}
}

func TestWhenPeopleLastUsedANetworkIsNoted(t *testing.T) {
	w := houseWorld(t, &house{})
	_, console := w.Created("ada@example.com", "initech")
	if since := time.Since(usedAt(t, w, "initech")); since > time.Minute {
		t.Errorf("a new network was last used %s ago", since)
	}

	w.Clock.Advance(2 * time.Hour)
	console.Get("/")
	if got := usedAt(t, w, "initech"); !got.Equal(w.Clock.Now()) {
		t.Errorf("after a visit, used at %s, want %s", got, w.Clock.Now())
	}
	// Written once an hour at most.
	visited := w.Clock.Now()
	w.Clock.Advance(10 * time.Minute)
	console.Get("/teams")
	if got := usedAt(t, w, "initech"); !got.Equal(visited) {
		t.Errorf("ten minutes on: used at %s, want %s still", got, visited)
	}

	// An IRC app signing in counts too.
	w.Clock.Advance(2 * time.Hour)
	if !SignsInOverIRC(t, w, "initech", "ada", "correct-horse-battery") { // as she made it with
		t.Fatal("ada couldn't sign in over IRC")
	}
	if got := usedAt(t, w, "initech"); !got.Equal(w.Clock.Now()) {
		t.Errorf("after an IRC login, used at %s, want %s", got, w.Clock.Now())
	}
}
