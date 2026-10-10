// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Getting started: the owner's org chart starts with a checklist that
// ticks itself off as they go.

func TestTheOwnerGetsAGettingStartedChecklistThatTicksItselfOff(t *testing.T) {
	w := NewWorld(t)
	gus := w.Browser("globex").SignedIn("gus") // globex's owner, on their own so far

	steps := Checklist(gus)
	for _, s := range []string{"talk", "invite", "irc", "goals", "budgets"} {
		if done, ok := steps[s]; !ok || done {
			t.Errorf("a new owner's checklist: %s is %v (listed %v), want listed and not done", s, done, ok)
		}
	}

	gus.Get("/people")
	gus.Post("/people/invite", url.Values{"role": {"Designer"}})
	gus.Get("/company")
	gus.Post("/company", url.Values{"description": {"Globex runs things."}})
	gus.Get("/")
	if r := gus.Post("/onboarding/irc", nil); r.Status != http.StatusSeeOther {
		t.Errorf("ticking off IRC apps: %d", r.Status)
	}

	steps = Checklist(gus)
	for s, want := range map[string]bool{"invite": true, "irc": true, "goals": true, "talk": false, "budgets": false} {
		if steps[s] != want {
			t.Errorf("after inviting, an IRC app, and a description: %s done = %v, want %v", s, steps[s], want)
		}
	}

	gus.Get("/")
	gus.Post("/onboarding/dismissed", nil)
	if steps := Checklist(gus); len(steps) != 0 {
		t.Errorf("after hiding it, the checklist still shows: %v", steps)
	}
}

func TestTheOwnerCanTickOffEveryStepThemselves(t *testing.T) {
	w := NewWorld(t)
	gus := w.Browser("globex").SignedIn("gus")
	all := []string{"talk", "invite", "irc", "goals", "budgets"}
	page := gus.Get("/").Body
	for _, s := range all {
		if !strings.Contains(page, `action="/onboarding/`+s+`"`) {
			t.Errorf("%s has no Done button", s)
		}
	}

	for i, s := range all {
		gus.Get("/")
		if r := gus.Post("/onboarding/"+s, nil); r.Status != http.StatusSeeOther {
			t.Fatalf("ticking off %s: %d", s, r.Status)
		}
		if i < len(all)-1 && !Checklist(gus)[s] {
			t.Errorf("%s isn't done after ticking it off", s)
		}
	}

	if steps := Checklist(gus); len(steps) != 0 {
		t.Errorf("with every step done, the checklist still shows: %v", steps)
	}
	gus.Get("/")
	if r := gus.Post("/onboarding/nonsense", nil); r.Status != http.StatusNotFound {
		t.Errorf("ticking off a step there isn't: %d, want 404", r.Status)
	}
}

func TestOnlyTheOwnerSeesTheChecklist(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")

	if steps := Checklist(bo); len(steps) != 0 {
		t.Errorf("bo sees the owner's checklist: %v", steps)
	}
	bo.Get("/")
	if r := bo.Post("/onboarding/dismissed", nil); r.Status != http.StatusForbidden {
		t.Errorf("bo hiding the owner's checklist: %d, want 403", r.Status)
	}
	if steps := Checklist(w.Browser("acme").SignedIn("ada")); len(steps) == 0 {
		t.Error("bo hid the owner's checklist")
	}
}
