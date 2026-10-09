// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// What only GuildNet's hosted edition has (its beta list, the #feedback it
// collects) comes through an extension; the open edition runs one network,
// made by whoever sets it up.

func TestAnExtensionSaysWhoMayCreateANetwork(t *testing.T) {
	var allowed atomic.Bool
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Extension = platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
			return platform.Hooks{MayCreate: func(ctx context.Context, p platform.Person, owned int) (platform.CreateAnswer, error) {
				if allowed.Load() {
					return platform.CreateAnswer{}, nil
				}
				return platform.CreateAnswer{Why: "Ask the house first.", Note: template.HTML(`<p class="house-note">The house decides.</p>`)}, nil
			}}
		})
	})
	ada := SignedInHome(w, "ada@example.com")

	page := ada.Get("/networks")
	if !strings.Contains(page.Body, `<p class="house-note">The house decides.</p>`) || strings.Contains(page.Body, `href="/networks/new"`) {
		t.Errorf("your networks, not allowed: want the house's note and no create button:\n%s", page.Body)
	}
	if r := ada.Get("/networks/new"); r.Status != http.StatusForbidden || !strings.Contains(r.Text(), "Ask the house first.") {
		t.Errorf("the new network form: %d %s", r.Status, r.Text())
	}
	ada.Get("/networks")
	if r := ada.Post("/networks", CreateForm("initech")); r.Status != http.StatusForbidden || !strings.Contains(r.Text(), "Ask the house first.") {
		t.Errorf("creating: %d %s", r.Status, r.Text())
	}

	allowed.Store(true)
	if !strings.Contains(ada.Get("/networks").Body, `href="/networks/new"`) {
		t.Error("allowed, but there's no create button")
	}
	ada.Get("/networks/new")
	if r := ada.Post("/networks", CreateForm("initech")); r.Status != http.StatusSeeOther {
		t.Errorf("creating, allowed: %d %s", r.Status, r.Text())
	}
}

// The open edition runs one network: whoever signs in first creates it,
// and then there's no creating another, by them or anyone.
func TestTheOpenEditionRunsOneNetwork(t *testing.T) {
	w := NewWorld(t)
	ada := SignedInHome(w, "ada@example.com")

	if strings.Contains(ada.Get("/networks").Text(), "private beta") {
		t.Error("the open edition talks about GuildNet's beta")
	}
	ada.Get("/networks/new")
	if r := ada.Post("/networks", CreateForm("initech")); r.Status != http.StatusSeeOther {
		t.Fatalf("creating the network: %d %s", r.Status, r.Text())
	}
	ada.WaitReady("initech")

	for name, b := range map[string]*Browser{"ada": ada, "bob": SignedInHome(w, "bob@example.com")} {
		if strings.Contains(b.Get("/networks").Body, `href="/networks/new"`) {
			t.Errorf("%s is offered another network", name)
		}
		if r := b.Get("/networks/new"); r.Status != http.StatusForbidden || !strings.Contains(r.Text(), "one network") {
			t.Errorf("%s at the new network form: %d %s", name, r.Status, r.Text())
		}
	}
}

func TestAnExtensionAddsStepsToTheChecklist(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Extension = platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
			return platform.Hooks{Checklist: []platform.ChecklistStep{{
				Key: "house", Title: "Meet the house", Hint: "Say hello in #house.", Link: "/chat/", LinkText: "Open chat",
				Done: func(r *http.Request, n platform.Network) bool { return false },
			}}}
		})
	})
	_, console := w.Created("ada@example.com", "initech")

	page := console.Get("/").Text()
	if !strings.Contains(page, "Meet the house") || !strings.Contains(page, "Ask one of your agents something") {
		t.Fatalf("the checklist: want the house's step beside the console's own:\n%s", page)
	}
	if r := console.Post("/onboarding/house", nil); r.Status != http.StatusSeeOther {
		t.Errorf("ticking off the house's step: %d", r.Status)
	}
	console.Get("/")
	if r := console.Post("/onboarding/feedback", nil); r.Status != http.StatusNotFound {
		t.Errorf("a step that isn't there: %d, want 404", r.Status)
	}
}

func TestTheOpenChecklistDoesntAskForFeedback(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")

	page := console.Get("/").Text()
	if !strings.Contains(page, "Ask one of your agents something") || strings.Contains(page, "#feedback") {
		t.Errorf("the open checklist:\n%s", page)
	}
}

func TestAnExtensionsJobsRunBesideTheConsolesOwn(t *testing.T) {
	var ran atomic.Int32
	p := platform.New(platform.Options{BaseDomain: "guildnet.test", Extension: platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
		return platform.Hooks{Jobs: []platform.Job{{Every: time.Minute, Run: func(context.Context) { ran.Add(1) }}}}
	})})

	jobs := p.Jobs()
	var mine *platform.Job
	for i, j := range jobs {
		if j.Every == time.Minute {
			mine = &jobs[i]
		}
	}
	if mine == nil || len(jobs) < 2 {
		t.Fatalf("jobs = %d, want the console's own and the extension's", len(jobs))
	}
	mine.Run(context.Background())
	if ran.Load() != 1 {
		t.Error("the extension's job didn't run")
	}
}
