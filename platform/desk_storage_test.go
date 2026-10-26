// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network's desks' files count against its storage. Its owner sees how
// much is used in Settings, each agent's page says what its own desk holds,
// whoever may change an agent can clear its desk, and the owner is told
// when the desks are nearly full, and when they're full.

const (
	gib          = int64(1) << 30
	storageEvery = 10 * time.Minute // the warnings' job
)

func storageUseWorld(t *testing.T) (*World, *desksVM, *Outbox) {
	t.Helper()
	vm, out := newDesksVM(t), &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: vm.srv.URL, Key: SandboxKey}
	})
	return w, vm, out
}

func TestTheOwnerSeesTheirDesksStorageInSettings(t *testing.T) {
	w, vm, _ := storageUseWorld(t)
	_, console := w.Created(ada, "initech")
	vm.setUse("initech", map[string]int64{"kai": 3 * gib, "vera": gib / 5})
	w.RunJobs(storageEvery)

	page := VisibleText(console.Get("/settings"))

	if !strings.Contains(page, "3.2 of 5 GB") {
		t.Errorf("Settings should say how much of the desks' storage is used:\n%s", page)
	}
}

// A page never waits on sandboxd, whose VM may be off: until sandboxd has
// answered lately, it isn't asked at all.
func TestAPageDoesntAskTheSandboxUnlessItAnsweredLately(t *testing.T) {
	w, vm, _ := storageUseWorld(t)
	_, console := w.Created(ada, "initech")

	page := VisibleText(console.Get("/settings"))

	if strings.Contains(page, "Desks' storage") || vm.storageAsked() != 0 {
		t.Errorf("Settings asked sandboxd (%d times) before it answered lately:\n%s", vm.storageAsked(), page)
	}
}

// The sandbox VM sleeps when no desk is working, and isn't woken to look:
// Settings shows what it last said.
func TestWhileTheSandboxSleepsSettingsShowWhatItLastSaid(t *testing.T) {
	w, vm, _ := storageUseWorld(t)
	_, console := w.Created(ada, "initech")
	vm.setUse("initech", map[string]int64{"kai": 2 * gib})
	w.RunJobs(storageEvery)

	vm.setDown(true)
	w.Clock.Advance(10 * time.Minute)
	page := VisibleText(console.Get("/settings"))

	if !strings.Contains(page, "2.0 of 5 GB") {
		t.Errorf("with the sandbox asleep, Settings should show the last use:\n%s", page)
	}
}

func TestAnAgentsPageShowsItsDeskAndWhoMayChangeItClearsIt(t *testing.T) {
	w, vm, _ := storageUseWorld(t)
	vm.setUse("acme", map[string]int64{"kai": 3 * gib / 2})
	w.RunJobs(storageEvery)
	ada := w.Browser("acme").SignedIn("ada")

	page := ada.Get("/staff/kai")
	if !strings.Contains(VisibleText(page), "1.5 GB") || !strings.Contains(page.Body, `action="/staff/kai/desk/clear"`) {
		t.Fatalf("kai's page should say what its desk holds, and offer clearing it:\n%s", VisibleText(page))
	}
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/staff/kai"); strings.Contains(page.Body, `action="/staff/kai/desk/clear"`) {
		t.Error("bo can't change kai, so shouldn't be offered clearing its desk")
	}
	if r := bo.Post("/staff/kai/desk/clear", nil); r.Status != http.StatusForbidden || vm.cleared("acme", "kai") {
		t.Fatalf("bo clearing kai's desk: %d", r.Status)
	}

	ada.Get("/staff/kai")
	r := ada.Post("/staff/kai/desk/clear", url.Values{})

	if r.Status != http.StatusSeeOther || !vm.cleared("acme", "kai") {
		t.Fatalf("ada clearing kai's desk: %d %s", r.Status, r.Text())
	}
	if page := VisibleText(ada.Get("/staff/kai")); strings.Contains(page, "1.5 GB") {
		t.Errorf("after clearing, kai's page still says its desk holds 1.5 GB:\n%s", page)
	}
}

// The owner hears once as the desks pass 80%, once as they fill, and again
// only after use drops back.
func TestTheOwnerIsToldWhenTheDesksAreNearlyFullAndFull(t *testing.T) {
	w, vm, out := storageUseWorld(t)
	w.Created(ada, "initech")
	told := func() []string {
		t.Helper()
		var subjects []string
		for _, m := range out.To(ada) {
			if strings.Contains(m.Subject, "storage") {
				subjects = append(subjects, m.Subject)
			}
		}
		return subjects
	}
	check := func(gb float64) {
		t.Helper()
		vm.setUse("initech", map[string]int64{"kai": int64(gb * float64(gib))})
		w.RunJobs(storageEvery) // it asks sandboxd afresh each time
	}

	check(3)
	if got := told(); len(got) != 0 {
		t.Fatalf("at 60%%: %q", got)
	}
	check(4.2)
	check(4.4)
	if got := told(); len(got) != 1 || !strings.Contains(got[0], "nearly full") {
		t.Fatalf("past 80%%, twice: %q, want one nearly-full email", got)
	}
	check(5)
	check(5)
	if got := told(); len(got) != 2 || !strings.Contains(got[1], "full") {
		t.Fatalf("full, twice: %q, want one full email more", got)
	}
	check(1)
	check(4.5)
	if got := told(); len(got) != 3 || !slices.ContainsFunc(got[2:], func(s string) bool { return strings.Contains(s, "nearly full") }) {
		t.Errorf("after dropping back and filling again: %q", got)
	}
}
