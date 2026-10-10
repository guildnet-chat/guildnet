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

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Bringing people into a network, and their passwords. The owner invites
// someone from the console: a one-time link, good for a week, that lets
// them pick a nick and password and join the org as the owner set it up.
// Everyone can change their own password; the owner can make a one-time
// reset link for someone who forgot theirs; and an owner who forgot their
// own sets a new one from the home page, signed in with Google.

// invite has ada, acme's owner, invite someone and returns the link's path.
func invite(t *testing.T, w *World, form url.Values) string {
	t.Helper()
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	r := ada.Post("/people/invite", form)
	if r.Status != http.StatusOK {
		t.Fatalf("inviting: %d %s", r.Status, r.Text())
	}
	m := JoinLink.FindString(r.Body)
	if m == "" {
		t.Fatalf("no invite link on the page:\n%s", r.Text())
	}
	return m
}

func TestTheOwnerInvitesSomeoneAndTheyJoinTheOrg(t *testing.T) {
	w := NewWorld(t)
	link := invite(t, w, url.Values{"role": {"Designer"}, "team": {"Engineering"}})

	dee := w.Browser("acme")
	page := dee.Get(link)
	if page.Status != http.StatusOK || !strings.Contains(page.Text(), "Designer") || !strings.Contains(page.Text(), "Engineering") {
		t.Fatalf("the invite page: %d %s", page.Status, page.Text())
	}
	r := dee.Post(link, url.Values{"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}})

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("joining: %d %s", r.Status, r.Text())
	}
	if pw, ok := w.Ergo.Password("acme", "dee"); !ok || pw != "a-long-enough-password" {
		t.Error("dee has no IRC account with the password she chose")
	}
	m, err := w.Networks["acme"].Store.Member(context.Background(), "dee")
	if err != nil || m.Role != "Designer" || m.Team != "Engineering" {
		t.Errorf("dee in the org: %+v %v", m, err)
	}
	if home := dee.Get("/"); home.Status != http.StatusOK || !strings.Contains(home.Text(), "dee") {
		t.Errorf("dee isn't signed in to the console: %d", home.Status)
	}
	// It worked once, and that's all.
	if again := w.Browser("acme").Get(link); again.Status != http.StatusGone {
		t.Errorf("the link a second time: %d, want 410", again.Status)
	}
}

func TestAnInviteIsOnlyGoodForAWeekAndOnlyOnItsNetwork(t *testing.T) {
	w := NewWorld(t)
	link := invite(t, w, url.Values{"role": {"Designer"}})

	if r := w.Browser("globex").Get(link); r.Status != http.StatusNotFound {
		t.Errorf("acme's link on globex: %d, want 404", r.Status)
	}
	w.Clock.Advance(7*24*time.Hour + time.Minute)
	r := w.Browser("acme").Get(link)
	if r.Status != http.StatusGone || !strings.Contains(r.Text(), "expired") {
		t.Errorf("after a week: %d %s", r.Status, r.Text())
	}
}

func TestATakenNickIsRefusedAndTheInviteStillWorks(t *testing.T) {
	w := NewWorld(t)
	link := invite(t, w, url.Values{"role": {"Designer"}})
	dee := w.Browser("acme")
	dee.Get(link)
	for _, nick := range []string{"kai", "zed", "bo"} { // an agent; an IRC account outside the org; a member
		r := dee.Post(link, url.Values{"nick": {nick}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}})
		if r.Status != http.StatusBadRequest {
			t.Errorf("joining as %s: %d, want refused", nick, r.Status)
		}
	}
	if pw, _ := w.Ergo.Password("acme", "zed"); pw != "zed-pw" {
		t.Error("zed's password was changed by someone joining as zed")
	}
	if _, err := w.Networks["acme"].Store.Member(context.Background(), "zed"); err == nil {
		t.Error("zed was left in the org")
	}
	r := dee.Post(link, url.Values{"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}})
	if r.Status != http.StatusSeeOther {
		t.Errorf("then as dee: %d %s", r.Status, r.Text())
	}
}

func TestJoiningNeedsAGoodPasswordTypedTwice(t *testing.T) {
	w := NewWorld(t)
	link := invite(t, w, url.Values{"role": {"Designer"}})
	dee := w.Browser("acme")
	dee.Get(link)
	for what, form := range map[string]url.Values{
		"short":      {"nick": {"dee"}, "password": {"short"}, "confirm": {"short"}},
		"mismatched": {"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-passwrd"}},
		"a bad nick": {"nick": {"dee!x"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}},
	} {
		if r := dee.Post(link, form); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", what, r.Status)
		}
	}
	if _, ok := w.Ergo.Password("acme", "dee"); ok {
		t.Error("dee's account was made from a bad form")
	}
}

// IRC passwords can't have spaces (Ergo refuses them), so every form that
// sets one says so, rather than failing somewhere further on.
func TestEveryPasswordFormSaysPasswordsCantHaveSpaces(t *testing.T) {
	w := NewWorld(t)
	spaced := url.Values{"password": {"has spaces in it"}, "confirm": {"has spaces in it"}}
	withNick := url.Values{"nick": {"dee"}, "password": spaced["password"], "confirm": spaced["confirm"]}
	withCurrent := url.Values{"current": {"cy-pw"}, "password": spaced["password"], "confirm": spaced["confirm"]}
	joinPath := invite(t, w, url.Values{"role": {"Designer"}})
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	resetPath := ResetLink.FindString(ada.Post("/people/cy/reset", nil).Body)
	cy := w.Browser("acme").SignedIn("cy")
	bob := w.CreateNetwork("bob@example.com", "bobco")

	check := func(what string, b *Browser, page, path string, form url.Values) {
		t.Helper()
		b.Get(page)
		r := b.Post(path, form)
		if r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "spaces") {
			t.Errorf("%s: %d, want 400 saying passwords can't have spaces", what, r.Status)
		}
	}
	check("joining", w.Browser("acme"), joinPath, joinPath, withNick)
	check("resetting", w.Browser("acme"), resetPath, resetPath, spaced)
	check("changing your own", cy, "/account", "/account/password", withCurrent)
	check("the owner, from the home page", bob, "/networks/bobco", "/networks/bobco/password", spaced)
	if _, ok := w.Ergo.Password("acme", "dee"); ok {
		t.Error("dee's account was made anyway")
	}
}

func TestTheOwnerCancelsAnInvite(t *testing.T) {
	w := NewWorld(t)
	link := invite(t, w, url.Values{"role": {"Designer"}})
	ada := w.Browser("acme").SignedIn("ada")
	page := ada.Get("/people").Body
	cancel := regexp.MustCompile(`action="(/people/invites/[^"]+/cancel)"`).FindStringSubmatch(page)
	if cancel == nil {
		t.Fatalf("no pending invite to cancel:\n%s", page)
	}

	if r := ada.Post(cancel[1], nil); r.Status != http.StatusSeeOther {
		t.Fatalf("cancelling: %d", r.Status)
	}
	if r := w.Browser("acme").Get(link); r.Status != http.StatusGone {
		t.Errorf("a cancelled invite: %d, want 410", r.Status)
	}
}

func TestEveryoneCanChangeTheirOwnPassword(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	elsewhere := w.Browser("acme").SignedIn("cy")
	cy.Get("/account")

	if r := cy.Post("/account/password", url.Values{"current": {"wrong"}, "password": {"cys-new-password"}, "confirm": {"cys-new-password"}}); r.Status != http.StatusUnauthorized {
		t.Errorf("with the wrong current password: %d, want 401", r.Status)
	}
	cy.Get("/account")
	r := cy.Post("/account/password", url.Values{"current": {"cy-pw"}, "password": {"cys-new-password"}, "confirm": {"cys-new-password"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("changing: %d %s", r.Status, r.Text())
	}

	if pw, _ := w.Ergo.Password("acme", "cy"); pw != "cys-new-password" {
		t.Error("cy's IRC password didn't change")
	}
	if r := cy.Get("/"); r.Status != http.StatusOK {
		t.Errorf("cy was signed out where she changed it: %d", r.Status)
	}
	if r := elsewhere.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("cy's other sign-in outlived her old password: %d", r.Status)
	}
}

func TestTheOwnerMakesAResetLinkForSomeoneWhoForgot(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	cyElsewhere := w.Browser("acme").SignedIn("cy")
	ada.Get("/people")
	r := ada.Post("/people/cy/reset", nil)
	link := ResetLink.FindString(r.Body)
	if r.Status != http.StatusOK || link == "" {
		t.Fatalf("making cy a reset link: %d %s", r.Status, r.Text())
	}

	cy := w.Browser("acme")
	if page := cy.Get(link); page.Status != http.StatusOK || !strings.Contains(page.Text(), "cy") {
		t.Fatalf("the reset page: %d %s", page.Status, page.Text())
	}
	if r := cy.Post(link, url.Values{"password": {"cys-new-password"}, "confirm": {"cys-new-password"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("resetting: %d %s", r.Status, r.Text())
	}
	if pw, _ := w.Ergo.Password("acme", "cy"); pw != "cys-new-password" {
		t.Error("cy's IRC password didn't change")
	}
	if r := cyElsewhere.Get("/"); r.Status != http.StatusSeeOther {
		t.Error("a sign-in from before the reset still works")
	}
	if r := w.Browser("acme").Get(link); r.Status != http.StatusGone {
		t.Errorf("the reset link a second time: %d, want 410", r.Status)
	}
}

func TestAResetLinkLastsADay(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	link := ResetLink.FindString(ada.Post("/people/cy/reset", nil).Body)

	w.Clock.Advance(24*time.Hour + time.Minute)

	if r := w.Browser("acme").Get(link); r.Status != http.StatusGone {
		t.Errorf("a day later: %d, want 410", r.Status)
	}
}

func TestOnlyTheOwnerMakesResetLinks(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	bo.Get("/people")

	if r := bo.Post("/people/cy/reset", nil); r.Status != http.StatusForbidden || ResetLink.MatchString(r.Body) {
		t.Errorf("bo making cy a reset link: %d, want 403", r.Status)
	}
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	if r := ada.Post("/people/zed/reset", nil); r.Status != http.StatusNotFound || ResetLink.MatchString(r.Body) {
		t.Errorf("a reset link for zed, who isn't in the org: %d, want 404", r.Status)
	}
}

func TestAnOwnerWhoForgotTheirPasswordSetsANewOneFromTheHomePage(t *testing.T) {
	w := NewWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	bob.Get("/networks/bobco")

	r := bob.Post("/networks/bobco/password", url.Values{"password": {"bobs-new-password"}, "confirm": {"bobs-new-password"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("setting a new password: %d %s", r.Status, r.Text())
	}
	if pw, _ := w.Ergo.Password("bobco", "ada"); pw != "bobs-new-password" {
		t.Error("the owner's IRC password didn't change")
	}
	eve := w.OnHome("eve@example.com")
	eve.Get("/networks")
	if r := eve.Post("/networks/bobco/password", url.Values{"password": {"eves-password-here"}, "confirm": {"eves-password-here"}}); r.Status != http.StatusNotFound {
		t.Errorf("someone else setting bobco's owner's password: %d, want 404", r.Status)
	}
}

func TestInvitesAreKeptAcrossARestart(t *testing.T) {
	w := NewWorldWithPostgres(t)
	link := invite(t, w, url.Values{"role": {"Designer"}, "team": {"Engineering"}})

	w.RestartPlatform()

	dee := w.Browser("acme")
	dee.Get(link)
	if r := dee.Post(link, url.Values{"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}}); r.Status != http.StatusSeeOther {
		t.Errorf("joining after a restart: %d %s", r.Status, r.Text())
	}
	if r := w.Browser("acme").Get(link); r.Status != http.StatusGone {
		t.Errorf("used, after a restart: %d, want 410", r.Status)
	}
}
