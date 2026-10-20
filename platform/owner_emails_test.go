// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network's owner hears by email (at the Google address they created it
// with) when it's ready, when someone joins, and whenever something that
// could hand it to someone else changes: its model keys, its GitHub
// connection, the owner's password, or the network being deleted. Security
// alerts can't be turned off, and never carry a secret.

func TestTheOwnerIsWelcomedWhenTheirNetworkIsReady(t *testing.T) {
	w, out := MailWorld(t)

	w.Created("ada@example.com", "initech")

	out.One(t, "ada@example.com", "initech is ready", "http://initech."+BaseDomain, "ada")
}

func TestTheOwnerHearsWhenSomeoneJoins(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	console.Get("/people")
	link := JoinLink.FindString(console.Post("/people/invite", url.Values{"role": {"Designer"}}).Body)
	dee := w.Browser("initech")
	dee.Get(link)

	dee.Post(link, url.Values{"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}})

	out.One(t, "ada@example.com", "dee joined initech", "Designer", "http://initech."+BaseDomain+"/people")
}

// A reset link lets whoever has it sign in as that person, so the owner
// hears when an admin makes one, and whenever one is used.
func TestTheOwnerHearsOfResetLinks(t *testing.T) {
	w, out := MailWorld(t)
	_, ada := w.Created("ada@example.com", "initech")
	for _, person := range []string{"bo", "cy"} {
		w.Ergo.AddAccount("initech", person, person+"-password-123")
		w.Host.AddMember(t, "initech", "ada", person)
	}
	ada.Get("/people/bo")
	if r := ada.Post("/people/bo/access", url.Values{"level": {"Admin"}, "give": {"1"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("making bo an admin: %d %s", r.Status, r.Text())
	}
	bo := w.Browser("initech")
	bo.SignIn("bo", "bo-password-123")
	bo.Get("/people")

	link := ResetLink.FindString(bo.Post("/people/cy/reset", nil).Body)

	out.One(t, "ada@example.com", "bo made a password reset link for cy", "initech", "If that wasn't expected")
	cy := w.Browser("initech")
	cy.Get(link)
	if r := cy.Post(link, url.Values{"password": {"cys-new-long-password"}, "confirm": {"cys-new-long-password"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("cy using the link: %d %s", r.Status, r.Text())
	}
	out.One(t, "ada@example.com", "cy's password on initech was reset", "the reset link bo made")
	// The owner's own links: no need to tell them they made one.
	ada.Get("/people")
	ada.Post("/people/bo/reset", nil)
	for _, m := range out.To("ada@example.com") {
		if strings.Contains(m.Subject, "ada made") {
			t.Errorf("the owner was told of their own link: %q", m.Subject)
		}
	}
}

// Whoever first sets up someone's second step holds their account from
// then on, so the owner hears when it's turned on, and when another way in
// is added to it, as when it's turned off.
func TestTheOwnerHearsWhenTwoStepIsTurnedOnOrAddedTo(t *testing.T) {
	w, out := MailWorld(t)
	w.Created("ada@example.com", "initech")
	for _, person := range []string{"bo", "cy"} {
		w.Ergo.AddAccount("initech", person, person+"-password-123")
		w.Host.AddMember(t, "initech", "ada", person)
	}
	bo := w.Browser("initech")
	bo.SignIn("bo", "bo-password-123")
	cy := w.Browser("initech")
	cy.SignIn("cy", "cy-password-123")

	bo.SetUpTwoStep()
	cy.AddPasskey(NewAuthenticator(t, "initech"), "/account/two-step")
	cy.AddPasskey(NewAuthenticator(t, "initech"), "/account")

	out.One(t, "ada@example.com", "Two-step sign-in for bo on initech was turned on", "an authenticator app", "If bo didn't")
	out.One(t, "ada@example.com", "Two-step sign-in for cy on initech was turned on", "a passkey")
	out.One(t, "ada@example.com", "cy added a passkey on initech")
}

func TestTheOwnerIsAlertedWhenModelKeysChange(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	console.Get("/settings")

	console.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-initech-0123456789"}})
	console.Get("/settings")
	console.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-initech-9876543210"}})
	console.Get("/settings")
	console.Post("/settings/keys/remove", url.Values{"provider": {"openai"}})

	for _, what := range []string{"added", "replaced", "removed"} {
		m := out.One(t, "ada@example.com", "OpenAI key was "+what, "ada", "If that wasn't you")
		for _, secret := range []string{"sk-proj-initech"} {
			if strings.Contains(m.Text+m.HTML, secret) {
				t.Errorf("the alert %q carries the key", m.Subject)
			}
		}
	}
}

func TestTheOwnerIsAlertedWhenTheirPasswordIsSetFromTheHomePage(t *testing.T) {
	w, out := MailWorld(t)
	home, _ := w.Created("ada@example.com", "initech")
	home.Get("/networks/initech")

	home.Post("/networks/initech/password", url.Values{"password": {"another-long-password"}, "confirm": {"another-long-password"}})

	m := out.One(t, "ada@example.com", "password", "ada", "initech", "If that wasn't you")
	if strings.Contains(m.Text+m.HTML, "another-long-password") {
		t.Error("the alert carries the password")
	}
}

func TestTheOwnerIsToldTheirNetworkWasDeleted(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	console.Get("/settings")

	console.Post("/settings/delete", url.Values{"confirm": {"initech"}})

	out.One(t, "ada@example.com", "initech was deleted", "ada")
}

func TestTheOwnerIsAlertedWhenGitHubIsConnectedOrDisconnected(t *testing.T) {
	gh := newFakeGitHubApp(t)
	key := appKey(t)
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), func(o *Setup) {
		o.GitHub = &platform.GitHubApp{AppID: 1, Slug: "guildnet", ClientID: "Iv1.test", ClientSecret: "client-secret", Key: key,
			APIURL: gh.srv.URL, WebURL: gh.srv.URL}
	})
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)

	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))
	bob.Get("/networks/bobco")
	bob.Post("/networks/bobco/github/disconnect", nil)

	out.One(t, "bob@example.com", "connected to GitHub", "acct7", "If that wasn't you")
	out.One(t, "bob@example.com", "disconnected from GitHub", "acct7")
}
