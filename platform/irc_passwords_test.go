// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// People's IRC passwords are the console's. Ergo asks it about every
// person's login (guildnet-ircauth, at the broker's /irc/login), and its
// answer is final; agents' accounts are Ergo's own ("not-mine"). A
// person's password still in Ergo, from before, moves to the console the
// first time they sign in. Guessing is held back the same way on IRC and
// in the console: five wrong from one address hold that address back for a
// while, and twenty from anywhere in an hour hold back every address that
// hasn't signed in as them lately, and tell the owner.

func TestTheConsoleSaysWhoSignsInOverIRC(t *testing.T) {
	w := NewWorld(t)

	right := IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.7")
	wrong := IRCLogin(t, w, "acme", "ada", "not-it", "198.51.100.7")

	if right != "yes" || wrong != "no" {
		t.Errorf("ada with her password: %q; with another: %q", right, wrong)
	}
}

// Ada's password was Ergo's; signing in moves it to the console, so a
// password left in Ergo is no way around the console.
func TestAPasswordMovesToTheConsoleAtSignIn(t *testing.T) {
	w := NewWorld(t)

	IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.7")

	if pw, _ := w.Ergo.Password("acme", "ada"); pw != "" {
		t.Errorf("ada's password is still Ergo's (%q)", pw)
	}
	if got := IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.7"); got != "yes" {
		t.Errorf("after moving, ada's password: %q", got)
	}
	b := w.Browser("acme")
	if r := b.SignIn("ada", "ada-pw"); r.Status != http.StatusSeeOther {
		t.Errorf("signing in to the console after: %d", r.Status)
	}
}

// A console with no broker (a developer's machine) has no Ergo asking it:
// Ergo keeps a copy of people's passwords to check.
func TestWhereErgoCantAskTheConsoleItKeepsACopy(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.ErgoAsks = false })
	b := w.Browser("acme")
	b.SignedIn("cy")
	b.Get("/account")

	b.Post("/account/password", url.Values{"current": {"cy-pw"}, "password": {"cys-new-password"}, "confirm": {"cys-new-password"}})

	if pw, _ := w.Ergo.Password("acme", "cy"); pw != "cys-new-password" {
		t.Errorf("Ergo's copy of cy's password is %q", pw)
	}
}

// Agents' accounts, and accounts that aren't anyone's, are Ergo's to check.
func TestAgentsLoginsAreErgos(t *testing.T) {
	w := NewWorld(t)

	for _, account := range []string{"kai", "nobody"} {
		if got := IRCLogin(t, w, "acme", account, "whatever", "10.0.0.5"); got != "not-mine" {
			t.Errorf("%s: %q, want not-mine", account, got)
		}
	}
}

func TestOnlyANetworksErgoAsksAboutItsLogins(t *testing.T) {
	w := NewWorld(t)

	status, _ := IRCLoginAs(t, w, "acme", "globex-ergo-secret", "ada", "ada-pw", "198.51.100.7")

	if status != http.StatusUnauthorized {
		t.Errorf("with another network's Ergo token: %d, want 401", status)
	}
}

func TestGuessingFromOneAddressIsHeldBack(t *testing.T) {
	w := NewWorld(t)
	for range 5 {
		IRCLogin(t, w, "acme", "ada", "a-guess", "203.0.113.5")
	}

	there := IRCLogin(t, w, "acme", "ada", "ada-pw", "203.0.113.5")
	elsewhere := IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.7")

	if there != "no" {
		t.Errorf("the guessing address, with the right password: %q, want no", there)
	}
	if elsewhere != "yes" {
		t.Errorf("another address: %q", elsewhere)
	}
	w.Clock.Advance(16 * time.Minute)
	if got := IRCLogin(t, w, "acme", "ada", "ada-pw", "203.0.113.5"); got != "yes" {
		t.Errorf("a quarter of an hour on: %q", got)
	}
}

func TestGuessingFromManyAddressesHoldsBackAllButKnownOnes(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, AnyoneCreates, WithMail(out))
	w.Created("ada@example.com", "initech")
	const pw = "correct-horse-battery" // CreateForm's
	if got := IRCLogin(t, w, "initech", "ada", pw, "198.51.100.7"); got != "yes" {
		t.Fatalf("ada signing in as usual: %q", got)
	}
	for i := range 20 {
		IRCLogin(t, w, "initech", "ada", "a-guess", fmt.Sprintf("203.0.113.%d", 10+i))
	}

	stranger := IRCLogin(t, w, "initech", "ada", pw, "192.0.2.1")
	known := IRCLogin(t, w, "initech", "ada", pw, "198.51.100.7")

	if stranger != "no" {
		t.Errorf("a new address, with the right password: %q, want no", stranger)
	}
	if known != "yes" {
		t.Errorf("ada's usual address: %q", known)
	}
	out.One(t, "ada@example.com", "guessing", "ada", "initech")
	w.Clock.Advance(61 * time.Minute)
	if got := IRCLogin(t, w, "initech", "ada", pw, "192.0.2.1"); got != "yes" {
		t.Errorf("an hour on, a new address: %q", got)
	}
}

// One address trying many accounts is held back too, but not from an
// account it has signed in as before: an office's people share an address.
func TestGuessingManyAccountsFromOneAddressIsHeldBack(t *testing.T) {
	t.Run("memory", func(t *testing.T) { sprayHeldBack(t, NewWorld(t)) })
	t.Run("postgres", func(t *testing.T) { sprayHeldBack(t, NewWorldWithPostgres(t)) })
}

func sprayHeldBack(t *testing.T, w *World) {
	t.Helper()
	b := w.Browser("acme")
	b.SignedIn("ada")
	b.Post("/signout", nil)
	for i := range 20 {
		b.SignIn(fmt.Sprintf("someone%d", i), "a-guess")
	}

	if r := b.SignIn("cy", "cy-pw"); r.Status != http.StatusTooManyRequests {
		t.Errorf("cy from the guessing address: %d, want held back", r.Status)
	}
	if got := IRCLogin(t, w, "acme", "cy", "cy-pw", "198.51.100.7"); got != "yes" {
		t.Errorf("cy from another address: %q", got)
	}
	if r := b.SignIn("ada", "ada-pw"); r.Status != http.StatusSeeOther {
		t.Errorf("ada, who has signed in from there before: %d", r.Status)
	}
	w.Clock.Advance(16 * time.Minute)
	if r := w.Browser("acme").SignIn("cy", "cy-pw"); r.Status != http.StatusSeeOther {
		t.Errorf("a quarter of an hour on: %d", r.Status)
	}
}

// Guessing over IRC holds back the console too, and the other way round.
func TestTheConsoleAndIRCShareTheLockout(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")
	for range 5 {
		b.SignIn("ada", "a-guess")
	}

	if got := IRCLogin(t, w, "acme", "ada", "ada-pw", BrowserAddress); got != "no" {
		t.Errorf("over IRC from the console's guessing address: %q, want no", got)
	}
	if r := b.SignIn("ada", "ada-pw"); r.Status == http.StatusSeeOther {
		t.Error("the console let ada in from the guessing address")
	}
}

// A restart (a deploy) forgets neither people's passwords nor guessing.
func TestPasswordsAndGuessingAreKeptAcrossARestart(t *testing.T) {
	w := NewWorldWithPostgres(t)
	IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.7") // moved to the console
	for range 5 {
		IRCLogin(t, w, "acme", "ada", "a-guess", "203.0.113.5")
	}

	w.RestartPlatform()

	if got := IRCLogin(t, w, "acme", "ada", "ada-pw", "203.0.113.5"); got != "no" {
		t.Errorf("the guessing address after a restart: %q, want no", got)
	}
	if got := IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.7"); got != "yes" {
		t.Errorf("ada after a restart: %q", got)
	}
	if pw, _ := w.Ergo.Password("acme", "ada"); pw != "" {
		t.Errorf("ada's password is Ergo's again (%q)", pw)
	}
}

func TestChangingAPasswordInTheConsoleChangesItForIRC(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")
	b.SignedIn("ada")
	b.Get("/account")

	if r := b.Post("/account/password", url.Values{"current": {"ada-pw"}, "password": {"a-new-long-password"}, "confirm": {"a-new-long-password"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("changing: %d %s", r.Status, r.Text())
	}

	if got := IRCLogin(t, w, "acme", "ada", "a-new-long-password", "198.51.100.7"); got != "yes" {
		t.Errorf("the new password: %q", got)
	}
	if got := IRCLogin(t, w, "acme", "ada", "ada-pw", "198.51.100.8"); got != "no" {
		t.Errorf("the old one: %q", got)
	}
}

func TestSomeoneRemovedFromTheOrgCantSignInOverIRC(t *testing.T) {
	w := NewWorld(t)
	IRCLogin(t, w, "acme", "bo", "bo-pw", "198.51.100.9") // moved to the console
	b := w.Browser("acme")
	b.SignedIn("ada")
	b.Get("/people/bo")

	if r := b.Post("/people/bo/remove", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("removing bo: %d %s", r.Status, r.Text())
	}

	if got := IRCLogin(t, w, "acme", "bo", "bo-pw", "198.51.100.9"); got == "yes" {
		t.Error("bo still signs in over IRC")
	}
	if pw, _ := w.Ergo.Password("acme", "bo"); pw == "bo-pw" {
		t.Error("bo's old password is back in Ergo")
	}
}
