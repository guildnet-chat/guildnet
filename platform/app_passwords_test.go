// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// IRC app passwords: an IRC app signs in with a password of its own, made
// on the account page and shown once, which can be revoked, ending the
// account's IRC sessions. An account with two-step sign-in signs in to IRC
// only with them, so its password alone can't read its chat.

func TestATwoStepAccountsPasswordAloneDoesntSignInOverIRC(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	if !SignsInOverIRC(t, w, "acme", "cy", "cy-pw") {
		t.Fatal("before two-step, cy's password should sign in over IRC")
	}

	cy.SetUpTwoStep()

	if SignsInOverIRC(t, w, "acme", "cy", "cy-pw") {
		t.Error("with two-step on, cy's password alone signed in over IRC")
	}
	if page := cy.Get("/account"); !strings.Contains(page.Text(), "IRC apps sign in with an app password") {
		t.Errorf("cy's account page should say IRC apps now need an app password:\n%s", page.Text())
	}
}

func TestAnAppPasswordSignsAnIRCAppIn(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	cy.SetUpTwoStep()

	_, pw := cy.MakeAppPassword("Laptop: WeeChat")

	if !SignsInOverIRC(t, w, "acme", "cy", pw) {
		t.Fatal("cy's app password didn't sign in over IRC")
	}
	page := cy.Get("/account")
	if !strings.Contains(page.Text(), "Laptop: WeeChat") || !strings.Contains(page.Text(), "last used") {
		t.Errorf("cy's account page should list the app password and when it was used:\n%s", page.Text())
	}
	if strings.Contains(page.Body, pw) {
		t.Error("the app password is shown again after it was made")
	}
	if SignsInOverIRC(t, w, "acme", "ada", pw) {
		t.Error("cy's app password signed in as ada")
	}
	if r := w.Browser("acme").SignIn("cy", pw); r.Status == http.StatusSeeOther {
		t.Error("an app password signed in to the console")
	}
}

// Anyone can keep their password out of IRC apps; it's still theirs too,
// without two-step.
func TestEveryoneCanMakeAppPasswords(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	_, pw := ada.MakeAppPassword("Phone")

	if !SignsInOverIRC(t, w, "acme", "ada", pw) || !SignsInOverIRC(t, w, "acme", "ada", "ada-pw") {
		t.Error("without two-step, ada's app password and her own should both sign in over IRC")
	}
}

func TestRevokingAnAppPasswordEndsTheAccountsIRCSessions(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	cy.SetUpTwoStep()
	id, pw := cy.MakeAppPassword("Old laptop")
	_, kept := cy.MakeAppPassword("New laptop")

	if r := cy.Post("/account/app-passwords/"+id+"/revoke", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("revoking: %d %s", r.Status, r.Text())
	}

	if SignsInOverIRC(t, w, "acme", "cy", pw) {
		t.Error("a revoked app password still signs in")
	}
	if !SignsInOverIRC(t, w, "acme", "cy", kept) {
		t.Error("revoking one app password stopped another")
	}
	if page := cy.Get("/account"); strings.Contains(page.Text(), "Old laptop") {
		t.Error("the revoked app password is still listed")
	}
	irc := w.Networks["acme"].IRC
	for deadline := time.Now().Add(5 * time.Second); !slices.ContainsFunc(irc.NickServ(), func(s string) bool { return strings.HasPrefix(s, "SUSPEND ADD cy") }) && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if !slices.ContainsFunc(irc.NickServ(), func(s string) bool { return strings.HasPrefix(s, "SUSPEND ADD cy") }) {
		t.Errorf("cy's IRC sessions weren't ended: NickServ was told %v", irc.NickServ())
	}
}

// Someone else can't revoke cy's.
func TestOnlyTheirOwnerRevokesAnAppPassword(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	id, pw := cy.MakeAppPassword("Laptop")
	ada := w.Browser("acme").SignedIn("ada")

	ada.Get("/account")
	ada.Post("/account/app-passwords/"+id+"/revoke", nil)

	if !SignsInOverIRC(t, w, "acme", "cy", pw) {
		t.Error("ada revoked cy's app password")
	}
}
