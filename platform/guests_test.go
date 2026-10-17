// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"regexp"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Whether guests may have agents work for them is the owner's call, since
// agents work with the company's accounts for whoever asks. The owner makes
// it on the People page.

var guestsAskOn = regexp.MustCompile(`<input type="checkbox" name="ask" checked`)

func TestTheOwnerLetsGuestsHaveAgentsWorkForThemFromThePeoplePage(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	page := ada.Get("/people")
	if !regexp.MustCompile(`action="/people/guests"`).MatchString(page.Body) || guestsAskOn.MatchString(page.Body) {
		t.Fatalf("the owner's People page has no guests switch, or it starts on:\n%s", page.Text())
	}

	r := ada.Post("/people/guests", url.Values{"ask": {"on"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("turning it on: %d %s", r.Status, r.Text())
	}
	if page := ada.Get("/people"); !guestsAskOn.MatchString(page.Body) {
		t.Errorf("after turning it on, it's off:\n%s", page.Text())
	}

	ada.Post("/people/guests", url.Values{})
	if page := ada.Get("/people"); guestsAskOn.MatchString(page.Body) {
		t.Error("after turning it off, it's on")
	}
}

func TestOnlyTheOwnerDecidesWhetherGuestsHaveAgentsWorkForThem(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/people"); regexp.MustCompile(`action="/people/guests"`).MatchString(page.Body) {
		t.Error("bo, who isn't the owner, has the guests switch")
	}

	if r := bo.Post("/people/guests", url.Values{"ask": {"on"}}); r.Status != http.StatusForbidden {
		t.Errorf("bo turning it on: %d, want 403", r.Status)
	}
	if page := w.Browser("acme").SignedIn("ada").Get("/people"); guestsAskOn.MatchString(page.Body) {
		t.Error("bo turned it on")
	}
}
