// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// GuildNet's own email, once it's set up to send some (through Resend): an
// admin's beta invite goes straight to the person, and so can a network's
// invite link. Without it, everything still works by hand, as before.

func TestANetworkInviteIsEmailedToThePerson(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out))
	ada := w.Browser("acme").SignedIn("ada")
	if !strings.Contains(ada.Get("/people?add=1").Body, `name="email"`) {
		t.Fatal("the invite form should offer to email the link")
	}

	r := ada.Post("/people/invite", url.Values{"role": {"Designer"}, "team": {"Engineering"}, "email": {" Dee@Example.com "}})

	if r.Status != http.StatusOK {
		t.Fatalf("inviting: %d %s", r.Status, r.Text())
	}
	link := JoinLink.FindString(r.Body)
	sent := out.To("dee@example.com")
	if link == "" || len(sent) != 1 {
		t.Fatalf("want the link on the page and one email to dee; link %q, %d emails", link, len(sent))
	}
	for _, want := range []string{link, "acme", "ada", "Designer", "Engineering"} {
		if !strings.Contains(sent[0].Text+sent[0].Subject, want) {
			t.Errorf("the invite email should say %q:\n%s\n%s", want, sent[0].Subject, sent[0].Text)
		}
	}
	if !strings.Contains(VisibleText(r), "Emailed it to dee@example.com") {
		t.Errorf("the page should say it was emailed:\n%s", r.Text())
	}

	if r := ada.Post("/people/invite", url.Values{"email": {"not an address"}}); r.Status != http.StatusBadRequest {
		t.Errorf("an invite to a non-address: %d, want 400", r.Status)
	}
}

// A network can't use GuildNet's email to write to the world: its invites
// by email are capped each day. Links can still be made.
func TestANetworksInviteEmailsAreCappedEachDay(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out))
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	for i := range 20 {
		if r := ada.Post("/people/invite", url.Values{"email": {"p" + Itoa(int64(i)) + "@example.com"}}); r.Status != http.StatusOK {
			t.Fatalf("invite %d: %d", i, r.Status)
		}
	}

	r := ada.Post("/people/invite", url.Values{"email": {"late@example.com"}})

	if r.Status != http.StatusTooManyRequests || len(out.To("late@example.com")) != 0 {
		t.Errorf("the 21st invite email today: %d, %d sent; want 429 and none", r.Status, len(out.To("late@example.com")))
	}
	if r := ada.Post("/people/invite", url.Values{"role": {"Designer"}}); r.Status != http.StatusOK || JoinLink.FindString(r.Body) == "" {
		t.Errorf("a link without an email should still be made: %d", r.Status)
	}
	w.Clock.Advance(24*time.Hour + time.Minute)
	if r := ada.Post("/people/invite", url.Values{"email": {"late@example.com"}}); r.Status != http.StatusOK {
		t.Errorf("the next day: %d", r.Status)
	}
}

// The emails look like the site (GuildNet's mark and indigo, a button for
// the one thing to do), and say the same as their plain text, which goes
// too. Whatever a customer typed (a role, a team) is shown as words, never
// as markup.
func TestEmailsLookLikeTheSite(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out))
	signIn, _ := w.Home().AskForLink(out, "eve@example.com")
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	r := ada.Post("/people/invite", url.Values{"role": {"<b>Boss</b>"}, "team": {"Engineering"}, "email": {"dee@example.com"}})
	link := "http://acme." + BaseDomain + JoinLink.FindString(r.Body)

	emails := map[string]struct {
		m    []mail.Message
		href string
	}{
		"a sign-in email":    {out.To("eve@example.com"), "http://" + BaseDomain + signIn},
		"a network's invite": {out.To("dee@example.com"), link},
	}
	for name, e := range emails {
		if len(e.m) != 1 {
			t.Fatalf("%s: %d sent", name, len(e.m))
		}
		m := e.m[0]
		for _, want := range []string{`<!doctype html>`, `name="color-scheme"`, `apple-touch-icon.png`, `#4c55d8`, `href="` + e.href} {
			if !strings.Contains(m.HTML, want) {
				t.Errorf("%s's HTML should have %s:\n%s", name, want, m.HTML)
			}
		}
		if !strings.Contains(m.Text, e.href) {
			t.Errorf("%s's text should have the link too:\n%s", name, m.Text)
		}
		if strings.Contains(m.HTML, "<script") || strings.Contains(m.HTML, "<link") {
			t.Errorf("%s's HTML should be self-contained: no scripts or stylesheets", name)
		}
	}
	if dee := out.To("dee@example.com"); len(dee) == 1 && (strings.Contains(dee[0].HTML, "<b>Boss") || !strings.Contains(dee[0].HTML, "&lt;b&gt;Boss")) {
		t.Error("the role should be shown as words, not markup")
	}
}

// Without email set up, the invite form doesn't offer it, and the admin
// sends beta invites by hand (see admin_test.go).
func TestWithoutEmailTheInviteFormDoesntOfferIt(t *testing.T) {
	w := NewWorld(t)
	if strings.Contains(w.Browser("acme").SignedIn("ada").Get("/people").Body, `name="email"`) {
		t.Error("the invite form offers email, but none can be sent")
	}
}
