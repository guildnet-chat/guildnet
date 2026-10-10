// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Passkeys as the second step: a phone, a laptop or a security key that
// proves it's the one added, bound to the network's own address. They can
// stand in for the authenticator app, or sit beside it.

var shownCodes = regexp.MustCompile(`class="recovery-code">([a-z2-7]{5}-[a-z2-7]{5})<`)

func TestAPasskeyAloneTurnsOnTwoStepSignIn(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	phone := NewAuthenticator(t, "acme")

	to := cy.AddPasskey(phone, "/account/two-step").To(t)

	codes := cy.Get(to)
	if n := len(shownCodes.FindAllString(codes.Body, -1)); n != 10 {
		t.Errorf("recovery codes shown on adding the first passkey: %d, want 10\n%s", n, codes.Text())
	}
	if again := cy.Get(to); len(shownCodes.FindAllString(again.Body, -1)) != 0 {
		t.Error("the recovery codes were shown twice")
	}
	if page := cy.Get("/account"); !strings.Contains(page.Text(), "Ada's phone") {
		t.Errorf("the account page should list the passkey:\n%s", page.Text())
	}

	b := w.Browser("acme")
	b.PasswordOnly("cy")
	if r := b.UsePasskey(phone); r.To(t) != "/" {
		t.Errorf("after the passkey: %s", r.Body)
	}
	if r := b.Get("/"); r.Status != http.StatusOK {
		t.Errorf("signed in with the passkey: %d", r.Status)
	}
}

func TestAPasskeyForAnotherSiteOrAnotherKeyIsRefused(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	phone := NewAuthenticator(t, "acme")
	cy.AddPasskey(phone, "/account/two-step").To(t)

	stranger := NewAuthenticator(t, "acme")
	stranger.ID = phone.ID // the same name, but not the same key
	b := w.Browser("acme")
	b.PasswordOnly("cy")
	if r := b.UsePasskey(stranger); r.Status == http.StatusOK {
		t.Error("a key that wasn't added signed in")
	}

	phished := *phone
	phished.Origin = "http://acme-login.example"
	c := w.Browser("acme")
	c.PasswordOnly("cy")
	if r := c.UsePasskey(&phished); r.Status == http.StatusOK {
		t.Error("the passkey signed in from another site")
	}
	if r := c.Get("/"); r.Status == http.StatusOK {
		t.Error("signed in anyway")
	}
}

func TestAPasskeyCanSitBesideTheApp(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	laptop := NewAuthenticator(t, "acme")

	if to := cy.AddPasskey(laptop, "/account").To(t); to != "/account" {
		t.Errorf("adding a second way in shows no new codes; went to %q", to)
	}

	b := w.Browser("acme")
	b.PasswordOnly("cy")
	if r := b.UsePasskey(laptop); r.Status != http.StatusOK {
		t.Errorf("the passkey: %d %s", r.Status, r.Body)
	}
	c := w.Browser("acme")
	c.PasswordOnly("cy")
	w.Clock.Advance(30 * time.Second)
	if r := c.SecondStep(TOTP(t, secret, w.Clock.Now())); r.Status != http.StatusSeeOther {
		t.Errorf("the app, beside the passkey: %d", r.Status)
	}
}

func TestTheLastWayInCantBeRemovedAlone(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	phone, laptop := NewAuthenticator(t, "acme"), NewAuthenticator(t, "acme")
	cy.AddPasskey(phone, "/account/two-step").To(t)
	cy.AddPasskey(laptop, "/account").To(t)
	page := cy.Get("/account")
	ids := regexp.MustCompile(`action="/account/passkeys/([A-Za-z0-9_-]+)/remove"`).FindAllStringSubmatch(page.Body, -1)
	if len(ids) != 2 {
		t.Fatalf("want two passkeys to remove:\n%s", page.Text())
	}

	if r := cy.Post("/account/passkeys/"+ids[0][1]+"/remove", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("removing one of two: %d %s", r.Status, r.Text())
	}
	cy.Get("/account")
	if r := cy.Post("/account/passkeys/"+ids[1][1]+"/remove", nil); r.Status != http.StatusBadRequest {
		t.Errorf("removing the last: %d, want refused (turn it off instead)", r.Status)
	}
}

func TestPasskeysAreKeptInEitherStore(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		steps := s.(platform.TwoSteps)
		s.AddNetwork(ctx, acmeNet)
		added := time.Unix(1_700_000_000, 0).UTC()
		steps.SetTwoStep(ctx, "acme", "cy", platform.TwoStepSetup{Recovery: []string{"h1"}, Since: added})
		for _, id := range []string{"k1", "k2"} {
			if err := steps.AddPasskey(ctx, "acme", "Cy", platform.Passkey{ID: id, Name: "Key " + id, Credential: []byte(`{"id":"` + id + `"}`), Added: added}); err != nil {
				t.Fatal(err)
			}
		}
		used := added.Add(time.Hour)
		if err := steps.UpdatePasskey(ctx, "acme", "cy", platform.Passkey{ID: "k2", Credential: []byte(`{"n":2}`), Used: used}); err != nil {
			t.Fatal(err)
		}
		// Keeping the app's setup doesn't touch the passkeys.
		steps.SetTwoStep(ctx, "acme", "cy", platform.TwoStepSetup{Secret: "JBSWY3DPEHPK3PXP", Recovery: []string{"h1"}, Since: added})

		got, _ := steps.TwoStepOf(ctx, "acme", "cy")
		if len(got.Passkeys) != 2 || got.Passkeys[1].Name != "Key k2" || string(got.Passkeys[1].Credential) != `{"n":2}` || !got.Passkeys[1].Used.Equal(used) || !got.Passkeys[0].Used.IsZero() {
			t.Errorf("kept: %+v", got.Passkeys)
		}
		steps.RemovePasskey(ctx, "acme", "cy", "k1")
		if got, _ := steps.TwoStepOf(ctx, "acme", "cy"); len(got.Passkeys) != 1 {
			t.Errorf("after removing one: %+v", got.Passkeys)
		}
		steps.RemoveTwoStep(ctx, "acme", "cy")
		if got, _ := steps.TwoStepOf(ctx, "acme", "cy"); got.On() {
			t.Errorf("after turning it off: %+v", got)
		}
	})
}

func TestSomeoneWhoMustCanSetUpWithAPasskey(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, RequireTwoStep)
	ada := w.Browser("acme").SignedIn("ada")
	if r := ada.Get("/"); r.Header.Get("Location") != "/account/two-step" {
		t.Fatalf("the owner without it: %q", r.Header.Get("Location"))
	}

	ada.AddPasskey(NewAuthenticator(t, "acme"), "/account/two-step").To(t)

	if r := ada.Get("/"); r.Status != http.StatusOK {
		t.Errorf("with a passkey: %d", r.Status)
	}
}
