// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Saving in the console says so: the page it lands on shows "Saved." once,
// and only when the save went through.

func TestASaveSaysItWasSavedOnce(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	bo.Get("/teams/Sales")

	if r := bo.Post("/teams/Sales", url.Values{"repos": {"", "docs"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("saving: %d %s", r.Status, r.Text())
	}
	landed := bo.Get("/teams/Sales")
	again := bo.Get("/teams/Sales")

	if !strings.Contains(landed.Body, `role="status"`) || !strings.Contains(landed.Text(), "Saved.") {
		t.Errorf("the page after saving should say it's saved:\n%s", landed.Text())
	}
	if strings.Contains(again.Text(), "Saved.") {
		t.Error("it should say so once, not on every visit after")
	}
}

func TestASaveThatFailedDoesntSayItWasSaved(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	bo.Get("/teams/Engineering")

	r := bo.Post("/teams/Engineering", url.Values{"repos": {"docs"}}) // bo doesn't lead Engineering
	after := bo.Get("/teams/Sales")

	if r.Status == http.StatusSeeOther {
		t.Fatalf("bo changed a team she doesn't lead: %d", r.Status)
	}
	if strings.Contains(r.Text(), "Saved.") || strings.Contains(after.Text(), "Saved.") {
		t.Error("a refused save mustn't say it was saved")
	}
}

// Removing something says it's done, not saved.
func TestRemovingSaysItsDone(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/staff/kai")

	if r := ada.Post("/staff/kai/fire", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("firing: %d %s", r.Status, r.Text())
	}

	if page := ada.Get("/"); !strings.Contains(page.Text(), "Done.") {
		t.Errorf("the page after firing should say it's done:\n%s", page.Text())
	}
}

// A button shows it was pressed while its form is on its way (forms.js),
// which every console page loads.
func TestConsolePagesLoadTheFormsScript(t *testing.T) {
	w := NewWorld(t)

	page := w.Browser("acme").SignedIn("ada").Get("/teams/Sales")

	if !strings.Contains(page.Body, "forms.") {
		t.Error("the console page doesn't load forms.js")
	}
}
