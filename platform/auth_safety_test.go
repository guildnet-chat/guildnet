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

	"github.com/guildnet-chat/guildnet/internal/org"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// An admin may make reset links for the people they invite, but never for
// the owner: that would hand them the owner's IRC account. The owner sets a
// new password from the home page.
func TestNobodyMakesTheOwnerAResetLink(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people/bo")
	if r := ada.Post("/people/bo/access", url.Values{"level": {"Admin"}, "give": {"1"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("making bo an admin: %d %s", r.Status, r.Text())
	}
	bo := w.Browser("acme").SignedIn("bo")
	bo.Get("/people")
	if r := bo.Post("/people/cy/reset", nil); r.Status != http.StatusOK || !ResetLink.MatchString(r.Body) {
		t.Fatalf("bo, an admin, making cy a reset link: %d %s", r.Status, r.Text())
	}
	bo.Get("/people")
	if r := bo.Post("/people/ada/reset", nil); r.Status != http.StatusForbidden || ResetLink.MatchString(r.Body) {
		t.Errorf("bo making the owner a reset link: %d, want 403", r.Status)
	}
	ada.Get("/people")
	if r := ada.Post("/people/ada/reset", nil); ResetLink.MatchString(r.Body) {
		t.Errorf("the owner making themselves one: %d", r.Status)
	}
}

// Whoever has a reset link signs in as that person, and can do what they
// can: nobody makes one for someone who may do more than they may.
func TestNobodyMakesAResetLinkForSomeoneWhoMayDoMore(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	w.Networks["acme"].Store.AddMember(context.Background(), org.Member{Account: "eli", Team: "Sales", ReportsTo: "bo"})
	for _, l := range []struct{ name, ability string }{{"Admin", "invite"}, {"Finance", "spend"}} {
		ada.Get("/access")
		if r := ada.Post("/access/"+l.name, url.Values{"abilities": {l.ability}, "scope": {"org"}}); r.Status != http.StatusSeeOther {
			t.Fatalf("setting %s: %d %s", l.name, r.Status, r.Text())
		}
	}
	give := func(person, level string) {
		t.Helper()
		ada.Get("/people/" + person)
		if r := ada.Post("/people/"+person+"/access", url.Values{"level": {level}, "give": {"1"}}); r.Status != http.StatusSeeOther {
			t.Fatalf("giving %s %s: %d %s", person, level, r.Status, r.Text())
		}
	}
	give("cy", "Admin")
	give("eli", "Finance")

	cy := w.Browser("acme").SignedIn("cy")
	cy.Get("/people")
	r := cy.Post("/people/eli/reset", nil)
	if r.Status != http.StatusForbidden || ResetLink.MatchString(r.Body) {
		t.Fatalf("cy, an admin who can't see spend, resetting eli, who can: %d, want 403 and no link", r.Status)
	}
	if !strings.Contains(r.Text(), "spend") {
		t.Errorf("should say what eli may do that cy may not:\n%s", r.Text())
	}
	cy.Get("/people")
	if r := cy.Post("/people/bo/reset", nil); r.Status != http.StatusForbidden || ResetLink.MatchString(r.Body) {
		t.Errorf("cy resetting bo, who leads Sales: %d, want 403 and no link", r.Status)
	}

	give("cy", "Finance")
	cy.Get("/people")
	if r := cy.Post("/people/eli/reset", nil); r.Status != http.StatusOK || !ResetLink.MatchString(r.Body) {
		t.Errorf("cy, now in Finance too, resetting eli: %d %s", r.Status, r.Text())
	}
}

// Taking someone out of the org takes them out of the network: their IRC
// password stops working (an IRC app could still sign in with it), and
// their console sign-ins end.
func TestSomeoneTakenOutCantSignInAnyMore(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people/cy")
	if r := ada.Post("/people/cy/remove", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("removing cy: %d %s", r.Status, r.Text())
	}

	if pw, _ := w.Ergo.Password("acme", "cy"); pw == Passwords["acme"]["cy"] {
		t.Error("cy's IRC password still works")
	}
	if r := cy.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("cy's sign-in from before: %d, want signed out", r.Status)
	}
}

// Someone at an unlocked computer, signed in, mustn't be able to swap in
// their own authenticator app or passkey: changing how someone signs in
// needs their second step shown recently, in this sign-in.
func TestChangingTheSecondStepNeedsItShownRecently(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	// Setting it up proved the app: adding a passkey straight after is fine.
	phone := NewAuthenticator(t, "acme")
	if to := cy.AddPasskey(phone, "/account").To(t); to != "/account" {
		t.Fatalf("adding a passkey just after setting up: %q", to)
	}

	// The app can't be swapped for another one.
	w.Clock.Advance(30 * time.Second)
	cy.Get("/account")
	other := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if r := cy.Post("/account/two-step", url.Values{"secret": {other}, "code": {TOTP(t, other, w.Clock.Now())}}); r.Status == http.StatusOK || r.Status == http.StatusSeeOther {
		t.Errorf("replacing the app: %d", r.Status)
	}

	// An hour on, the same sign-in can't add or remove passkeys.
	w.Clock.Advance(time.Hour)
	laptop := NewAuthenticator(t, "acme")
	cy.Get("/account")
	if begin := cy.Call("/account/passkeys/begin", nil); begin.Status == http.StatusOK {
		t.Errorf("adding a passkey an hour after signing in: %d", begin.Status)
	}
	page := cy.Get("/account")
	ids := regexp.MustCompile(`action="/account/passkeys/([A-Za-z0-9_-]+)/remove"`).FindAllStringSubmatch(page.Body, -1)
	if len(ids) != 1 {
		t.Fatalf("want one passkey:\n%s", page.Text())
	}
	if r := cy.Post("/account/passkeys/"+ids[0][1]+"/remove", nil); r.Status == http.StatusSeeOther {
		t.Errorf("removing a passkey an hour after signing in: %d", r.Status)
	} else if !strings.Contains(r.Text(), "Sign in again") {
		t.Errorf("should say to sign in again:\n%s", r.Text())
	}

	// Signed in again, with the second step, it can.
	cy.Post("/signout", nil)
	cy.PasswordOnly("cy")
	if r := cy.SecondStep(TOTP(t, secret, w.Clock.Now())); r.Status != http.StatusSeeOther {
		t.Fatalf("signing in again: %d %s", r.Status, r.Text())
	}
	if to := cy.AddPasskey(laptop, "/account").To(t); to != "/account" {
		t.Errorf("adding a passkey after signing in again: %q", to)
	}
}
