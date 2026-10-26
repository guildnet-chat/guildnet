// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/org"
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

// A guest (a contractor, a client) sees their channels and the agents in
// them, and nothing of the company's directory: no org chart, people, teams,
// goals or budgets.
func TestAGuestSeesTheirChannelsAndTheAgentsInThem(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].Store.AddMember(context.Background(), org.Member{Account: "zed", Role: "Contractor", Team: "Sales", Guest: true})
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/staff/kai")
	if r := ada.Post("/staff/kai", url.Values{"channels": {"#engineering"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("moving kai to #engineering only: %d %s", r.Status, r.Text())
	}

	zed := w.Browser("acme").SignedIn("zed")
	home := zed.Get("/")
	if home.Status != http.StatusOK {
		t.Fatalf("zed's home: %d %s", home.Status, home.Text())
	}
	for _, want := range []string{"#general", "#sales", "Vera", "Team lead"} {
		if !strings.Contains(home.Text(), want) {
			t.Errorf("zed's home should show %q:\n%s", want, home.Text())
		}
	}
	for _, not := range []string{"Kai", "Engineering", "Head of Sales", "Founder"} {
		if strings.Contains(home.Text(), not) {
			t.Errorf("zed's home shows %q:\n%s", not, home.Text())
		}
	}
	for _, link := range []string{`href="/people"`, `href="/teams"`, `href="/company"`, `href="/approvals"`, `href="/connectors"`} {
		if strings.Contains(home.Body, link) {
			t.Errorf("a guest's console links to %s", link)
		}
	}
	if !strings.Contains(home.Body, `href="/chat/"`) {
		t.Error("a guest's console should link to chat")
	}

	for _, path := range []string{"/people", "/people/bo", "/teams", "/teams/Sales", "/company", "/staff/vera", "/access", "/log", "/spend", "/roles", "/approvals", "/connectors"} {
		if r := zed.Get(path); r.Status != http.StatusForbidden || strings.Contains(r.Text(), "Head of Sales") {
			t.Errorf("zed opening %s: %d, want 403", path, r.Status)
		}
	}
	if r := zed.Get("/account"); r.Status != http.StatusOK {
		t.Errorf("zed's account page: %d", r.Status)
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
