// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network's settings, in its owner's console: timezone, model keys
// and deleting it.

func TestTheOwnerChangesTheNetworksTimezone(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	page := console.Get("/settings")
	if page.Status != http.StatusOK || !strings.Contains(page.Text(), "Europe/Paris") {
		t.Fatalf("settings: %d\n%s", page.Status, page.Text())
	}
	if !strings.Contains(console.Get("/").Text(), "Settings") {
		t.Error("the owner's console has no Settings")
	}
	r := console.Post("/settings", url.Values{"timezone": {"America/Edmonton"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("saving: %d %s", r.Status, r.Text())
	}
	if s, _ := w.Host.Spec("initech"); s.Timezone != "America/Edmonton" || s.ModelKeys[models.Anthropic] != "sk-ant-api03-test-key-0123456789" {
		t.Errorf("the host has timezone %q (keys %d)", s.Timezone, len(s.ModelKeys))
	}
	if !strings.Contains(console.Get("/settings").Text(), "America/Edmonton") {
		t.Error("the settings page doesn't show the new timezone")
	}
	console.Get("/settings")
	if r := console.Post("/settings", url.Values{"timezone": {"Mars/Olympus"}}); r.Status != http.StatusBadRequest {
		t.Errorf("an unknown timezone: %d", r.Status)
	}
}

func TestOnlyTheOwnerHasSettings(t *testing.T) {
	w := NewWorld(t)
	w.Created("ada@example.com", "initech")
	// ada makes bo a member of initech, as she could in IRC.
	w.Ergo.AddAccount("initech", "bo", "bo-password-123")
	w.Host.AddMember(t, "initech", "ada", "bo")
	bo := w.Browser("initech")
	if r := bo.SignIn("bo", "bo-password-123"); r.Status != http.StatusSeeOther {
		t.Fatalf("bo signing in: %d", r.Status)
	}
	if r := bo.Get("/settings"); r.Status != http.StatusForbidden {
		t.Errorf("bo opened settings: %d", r.Status)
	}
	if strings.Contains(bo.Get("/").Text(), "Settings") {
		t.Error("bo's console offers Settings")
	}
}

func TestAHandMadeNetworkHasNoSettingsHere(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	if r := ada.Get("/settings"); r.Status != http.StatusNotFound {
		t.Errorf("acme (set up by hand) has settings: %d", r.Status)
	}
}

func TestDeletingANetworkTakesItAwayCompletely(t *testing.T) {
	w := NewWorld(t)
	home, console := w.Created("ada@example.com", "initech")
	// Deleting ends the console, so it's a whole new page, not one swapped
	// in beside the sidebar and chat.
	if !regexp.MustCompile(`<form[^>]*action="/settings/delete"[^>]*hx-boost="false"`).MatchString(console.Get("/settings").Body) {
		t.Error("the delete form is swapped into the page")
	}
	console.Get("/settings")
	if r := console.Post("/settings/delete", url.Values{"confirm": {"initech2"}}); r.Status != http.StatusBadRequest {
		t.Errorf("deleting with the wrong name: %d", r.Status)
	}
	if len(w.Host.Removed) > 0 {
		t.Fatal("removed without the right name")
	}
	console.Get("/settings")
	// It answers straight away, on the same address (htmx's boosted forms
	// can't follow a redirect to another), with the way home; the host's
	// work carries on in the background.
	r := console.Post("/settings/delete", url.Values{"confirm": {"initech"}})
	if r.Status != http.StatusOK || !strings.Contains(r.Text(), "initech is being deleted") || !strings.Contains(r.Body, `href="http://`+BaseDomain+`/networks"`) {
		t.Fatalf("deleting: %d %s", r.Status, r.Text())
	}
	if strings.Contains(r.Text(), "Sign out") {
		t.Error("the deleted page still shows the network's console")
	}
	w.Host.WaitRemoved(t, "initech")
	if r := console.Get("/"); r.Status != http.StatusNotFound {
		t.Errorf("initech's console after deleting: %d", r.Status)
	}
	if strings.Contains(home.Get("/networks").Text(), "initech") {
		t.Error("your networks still lists initech")
	}
	// In the beta, that frees ada to create another.
	home.Get("/networks/new")
	if r := home.Post("/networks", CreateForm("initrode")); r.Status != http.StatusSeeOther {
		t.Errorf("creating another after deleting: %d %s", r.Status, r.Text())
	}
}

func TestAFailedNetworkCanBeRemovedFromItsSetupPage(t *testing.T) {
	w := NewWorld(t)
	w.Host.FailWith(ErrDiskFull)
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	b.Post("/networks", CreateForm("initech"))
	b.WaitReady("initech")
	w.Host.FailWith(nil)
	b.Get("/networks/initech")
	if r := b.Post("/networks/initech/remove", url.Values{"confirm": {"nope"}}); r.Status != http.StatusBadRequest {
		t.Errorf("removing with the wrong name: %d", r.Status)
	}
	b.Get("/networks/initech")
	if r := b.Post("/networks/initech/remove", url.Values{"confirm": {"initech"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("removing: %d %s", r.Status, r.Text())
	}
	w.Host.WaitRemoved(t, "initech")
	if r := b.Get("/networks/initech"); r.Status != http.StatusNotFound {
		t.Errorf("after removing: %d", r.Status)
	}
}

func TestANetworkBeingDeletedServesNothing(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	chat := w.Browser("initech")
	chat.Cookies = map[string]*http.Cookie{}
	for k, v := range console.Cookies {
		chat.Cookies[k] = v
	}
	w.Host.Hold() // the host takes its time removing it
	console.Get("/settings")
	console.Post("/settings/delete", url.Values{"confirm": {"initech"}})
	if r := chat.Get("/"); r.Status != http.StatusNotFound {
		t.Errorf("initech's console while it's being deleted: %d", r.Status)
	}
	w.Host.Release()
	w.Host.WaitRemoved(t, "initech")
}

// A network the server's admins suspended (for abuse, say) is left as it is
// for them: its owner can't delete it, or set it up again, from the home
// page. Its page still says what's happened.
func TestASuspendedNetworkIsntChangedFromTheHomePage(t *testing.T) {
	w := NewWorld(t)
	home, _ := w.Created("ada@example.com", "initech")
	n, _ := w.Store.Network(context.Background(), "initech")
	n.Status = platform.Suspended
	w.Store.UpdateNetwork(context.Background(), n)

	page := home.Get("/networks/initech")
	if page.Status != http.StatusOK || !strings.Contains(page.Text(), "initech is suspended") || strings.Contains(page.Body, `action="/networks/initech/remove"`) {
		t.Errorf("the suspended network's page: %d\n%s", page.Status, page.Text())
	}
	for path, form := range map[string]url.Values{
		"/networks/initech/remove": {"confirm": {"initech"}},
		"/networks/initech/retry":  {"password": {"a-long-irc-password"}},
	} {
		home.Get("/networks/initech")
		if r := home.Post(path, form); r.Status != http.StatusForbidden || !strings.Contains(r.Text(), "suspended") {
			t.Errorf("%s: %d %s", path, r.Status, r.Text())
		}
	}
	if len(w.Host.Removed) > 0 {
		t.Error("the suspended network was removed")
	}
}

func TestADeletionThatFailsCanBeTriedAgainFromHome(t *testing.T) {
	w := NewWorld(t)
	home, console := w.Created("ada@example.com", "initech")
	w.Host.FailWith(ErrDiskFull)
	console.Get("/settings")
	console.Post("/settings/delete", url.Values{"confirm": {"initech"}})
	var page Response
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if page = home.Get("/networks/initech"); strings.Contains(page.Text(), "didn't finish") {
			break
		}
	}
	if !strings.Contains(page.Text(), "Deleting it didn't finish") || strings.Contains(page.Text(), "no space left") {
		t.Fatalf("after a failed deletion:\n%s", page.Text())
	}
	w.Host.FailWith(nil)
	home.Get("/networks/initech")
	if r := home.Post("/networks/initech/remove", url.Values{"confirm": {"initech"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("trying again: %d %s", r.Status, r.Text())
	}
	w.Host.WaitRemoved(t, "initech")
}
