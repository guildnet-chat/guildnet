// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Signing in on the home page by email, beside Google: GuildNet emails a
// link that works once, within 15 minutes, in the browser that asked, and a
// code to type there instead (for reading the email on another device).
// Whichever way someone signs in, the same address is the same person.

func TestSigningInWithAnEmailedLink(t *testing.T) {
	w, out := MailWorld(t)
	b := w.Home()
	if page := b.Get("/signin"); !strings.Contains(page.Text(), "Email me a sign-in link") || !strings.Contains(page.Text(), "Sign in with Google") {
		t.Errorf("the sign-in page should offer email beside Google:\n%s", page.Text())
	}

	link, _ := b.AskForLink(out, "Dee@Example.com")

	if r := b.Get(link); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks" {
		t.Fatalf("the link: %d %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if page := b.Get("/networks"); page.Status != http.StatusOK || !strings.Contains(page.Text(), "dee@example.com") {
		t.Errorf("signed in as dee: %d\n%s", page.Status, page.Text())
	}
}

func TestAnEmailedLinkWorksOnlyInTheBrowserThatAskedAndOnlyOnce(t *testing.T) {
	w, out := MailWorld(t)
	asked := w.Home()
	link, _ := asked.AskForLink(out, "dee@example.com")

	elsewhere := w.Home()
	r := elsewhere.Get(link)
	if r.Status == http.StatusSeeOther || !strings.Contains(r.Text(), "browser where you asked") {
		t.Errorf("the link in another browser: %d %s", r.Status, r.Text())
	}
	if page := elsewhere.Get("/networks"); page.Status != http.StatusSeeOther {
		t.Errorf("another browser got signed in: %d", page.Status)
	}

	if r := asked.Get(link); r.Status != http.StatusSeeOther {
		t.Fatalf("the link where it was asked for, after another browser tried it: %d %s", r.Status, r.Text())
	}
	asked.Post("/signout", nil)
	if r := asked.Get(link); r.Status == http.StatusSeeOther {
		t.Error("the link worked twice")
	}
}

func TestTheEmailedCodeSignsInTheBrowserThatAsked(t *testing.T) {
	w, out := MailWorld(t)
	b := w.Home()
	_, code := b.AskForLink(out, "dee@example.com")

	if r := b.Post("/signin/email/code", url.Values{"code": {"000000"}}); r.Status != http.StatusUnauthorized || !strings.Contains(r.Text(), "didn't match") {
		t.Errorf("a wrong code: %d %s", r.Status, r.Text())
	}
	if r := b.Post("/signin/email/code", url.Values{"code": {code[:3] + " " + code[3:]}}); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks" {
		t.Fatalf("the code: %d %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}

	other := w.Home()
	other.Get("/signin")
	if r := other.Post("/signin/email/code", url.Values{"code": {code}}); r.Status == http.StatusSeeOther {
		t.Error("the code signed in a browser that didn't ask")
	}
}

func TestFiveWrongCodesVoidTheSignIn(t *testing.T) {
	w, out := MailWorld(t)
	b := w.Home()
	link, code := b.AskForLink(out, "dee@example.com")
	for range 5 {
		b.Post("/signin/email/code", url.Values{"code": {"000000"}})
	}

	if r := b.Post("/signin/email/code", url.Values{"code": {code}}); r.Status == http.StatusSeeOther {
		t.Error("the right code after five wrong ones")
	}
	if r := b.Get(link); r.Status == http.StatusSeeOther {
		t.Error("the link after five wrong codes")
	}
}

func TestAnEmailedLinkExpiresIn15Minutes(t *testing.T) {
	w, out := MailWorld(t)
	b := w.Home()
	link, _ := b.AskForLink(out, "dee@example.com")

	w.Clock.Advance(16 * time.Minute)

	if r := b.Get(link); r.Status == http.StatusSeeOther || !strings.Contains(r.Text(), "expired") {
		t.Errorf("after 16 minutes: %d %s", r.Status, r.Text())
	}
}

func TestTheSameAddressIsTheSamePersonWithGoogleOrEmail(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), AnyoneCreates)
	w.CreateNetwork("ada@example.com", "initech") // signed in with Google

	b := w.Home()
	link, _ := b.AskForLink(out, "ada@example.com")
	b.Get(link)

	if page := b.Get("/networks"); !strings.Contains(page.Text(), "initech") {
		t.Errorf("by email, ada should see the network she made with Google:\n%s", page.Text())
	}

	// And the other way round: by email first, then Google.
	c := w.Home()
	link, _ = c.AskForLink(out, "bo@example.com")
	c.Get(link)
	c.Get("/networks/new")
	form := CreateForm("bobco")
	form.Set("nick", "bo")
	if r := c.Post("/networks", form); r.Status != http.StatusSeeOther {
		t.Fatalf("bo creating a network: %d %s", r.Status, r.Text())
	}
	g := w.Home()
	g.SignInWithGoogle("bo@example.com")
	if page := g.Get("/networks"); !strings.Contains(page.Text(), "bobco") {
		t.Errorf("with Google, bo should see the network made by email:\n%s", page.Text())
	}
}

func TestSignInEmailsAreLimited(t *testing.T) {
	w, out := MailWorld(t)
	b := w.Home()
	for range 5 {
		b.AskForLink(out, "dee@example.com")
	}

	b.Get("/signin")
	r := b.Post("/signin/email", url.Values{"email": {"dee@example.com"}})

	if r.Status != http.StatusTooManyRequests || len(out.To("dee@example.com")) != 5 {
		t.Errorf("a sixth in the hour: %d, %d sent", r.Status, len(out.To("dee@example.com")))
	}
	w.Clock.Advance(time.Hour)
	b.AskForLink(out, "dee@example.com")
}

func TestOneVisitorCantEmailTheWholeWorld(t *testing.T) {
	w, out := MailWorld(t)
	b := w.Home()
	for i := range 20 {
		b.AskForLink(out, "person"+string(rune('a'+i))+"@example.com")
	}

	b.Get("/signin")
	if r := b.Post("/signin/email", url.Values{"email": {"another@example.com"}}); r.Status != http.StatusTooManyRequests {
		t.Errorf("a 21st address from one visitor: %d", r.Status)
	}
}

func TestSomeoneBlockedCantSignInByEmail(t *testing.T) {
	w, out := MailWorld(t)
	eve, _ := w.Store.EmailPerson(context.Background(), "eve@example.com")
	w.Store.SetBlocked(context.Background(), eve.ID, true)
	b := w.Home()
	link, _ := b.AskForLink(out, "eve@example.com")

	if r := b.Get(link); r.Status != http.StatusForbidden {
		t.Errorf("a blocked person's link: %d", r.Status)
	}
}

func TestWithoutEmailThereIsNoEmailSignIn(t *testing.T) {
	w := NewWorld(t)
	b := w.Home()
	if page := b.Get("/signin"); strings.Contains(page.Text(), "Email me a sign-in link") {
		t.Error("offered with no way to send it")
	}
	if r := b.Post("/signin/email", url.Values{"email": {"dee@example.com"}}); r.Status != http.StatusNotFound {
		t.Errorf("asking anyway: %d", r.Status)
	}
}

func TestAPersonIsFoundByTheirAddress(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		byEmail, err := s.EmailPerson(ctx, "Dee@Example.com")
		if err != nil || byEmail.Email != "dee@example.com" {
			t.Fatalf("a new address: %+v %v", byEmail, err)
		}
		if again, _ := s.EmailPerson(ctx, "dee@example.com"); again.ID != byEmail.ID {
			t.Error("the same address twice is two people")
		}
		if google, _ := s.GooglePerson(ctx, "sub-dee", "dee@example.com"); google.ID != byEmail.ID {
			t.Error("Google with the same address is another person")
		}
		if other, _ := s.GooglePerson(ctx, "sub-dee-2", "dee@example.com"); other.ID == byEmail.ID {
			t.Error("a second Google account took over the person")
		}
		ada, _ := s.GooglePerson(ctx, "sub-ada", "ada@example.com")
		if byEmail, _ := s.EmailPerson(ctx, "ADA@example.com"); byEmail.ID != ada.ID {
			t.Error("by email, ada is someone else")
		}
	})
}
