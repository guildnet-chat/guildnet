// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Signing in to guildnet.chat (with Google or an emailed link) is one step:
// whoever has the mailbox or the Google account. Owners reset their console
// passwords and two-step from here, and admins run GuildNet, so an
// authenticator app's code is asked for too, by everyone.

const ada = "ada@example.com"

func requireHomeTwoStep(o *Setup) { o.RequireHomeTwoStep = true }

func homeTwoStepWorld(t *testing.T) (*World, *Outbox) {
	t.Helper()
	out := &Outbox{}
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), requireHomeTwoStep), out
}

func TestEveryoneSetsUpAnAuthenticatorBeforeAnythingElse(t *testing.T) {
	w, _ := homeTwoStepWorld(t)
	b := w.Home()

	r := b.SignInWithGoogle(ada)
	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
		t.Fatalf("after Google: %d to %q, want setting up the app", r.Status, r.Header.Get("Location"))
	}
	for _, path := range []string{"/networks", "/networks/new"} {
		if r := b.Get(path); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
			t.Errorf("%s before setting it up: %d to %q", path, r.Status, r.Header.Get("Location"))
		}
	}
	b.Get("/networks/new")
	if r := b.Post("/networks", CreateForm("initech")); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
		t.Errorf("creating a network before setting it up: %d to %q", r.Status, r.Header.Get("Location"))
	}

	b.SetUpHomeTwoStep()

	if r := b.Get("/networks"); r.Status != http.StatusOK {
		t.Errorf("after setting it up: %d", r.Status)
	}
}

func TestTheAuthenticatorIsAskedAfterGoogle(t *testing.T) {
	w, _ := homeTwoStepWorld(t)
	first := w.Home()
	first.SignInWithGoogle(ada)
	secret, _ := first.SetUpHomeTwoStep()

	b := w.Home()
	r := b.SignInWithGoogle(ada)
	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin/two-step" {
		t.Fatalf("after Google: %d to %q, want the second step", r.Status, r.Header.Get("Location"))
	}
	if r := b.Get("/networks"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("between the steps: %d to %q, want still signed out", r.Status, r.Header.Get("Location"))
	}
	if r := b.HomeSecondStep("000000"); r.Status != http.StatusUnauthorized || !strings.Contains(r.Text(), "didn't match") {
		t.Errorf("a wrong code: %d %s", r.Status, r.Text())
	}
	w.Clock.Advance(30 * time.Second)
	if r := b.HomeSecondStep(TOTP(t, secret, w.Clock.Now())); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks" {
		t.Fatalf("the right code: %d %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if r := b.Get("/networks"); r.Status != http.StatusOK {
		t.Errorf("signed in: %d", r.Status)
	}
}

// An emailed link is no more than Google: the app's asked for after it.
func TestTheAuthenticatorIsAskedAfterAnEmailedLink(t *testing.T) {
	w, out := homeTwoStepWorld(t)
	first := w.Home()
	link, _ := first.AskForLink(out, "dee@example.com")
	if r := first.Get(link); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
		t.Fatalf("the link, the first time: %d to %q", r.Status, r.Header.Get("Location"))
	}
	first.SetUpHomeTwoStep()

	b := w.Home()
	link, _ = b.AskForLink(out, "dee@example.com")
	if r := b.Get(link); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin/two-step" {
		t.Fatalf("the link, with the app set up: %d to %q", r.Status, r.Header.Get("Location"))
	}
	if r := b.Get("/networks"); r.Status == http.StatusOK {
		t.Error("an emailed link alone signed in")
	}
}

func TestAHomeRecoveryCodeSignsInOnce(t *testing.T) {
	w, _ := homeTwoStepWorld(t)
	first := w.Home()
	first.SignInWithGoogle(ada)
	_, codes := first.SetUpHomeTwoStep()

	b := w.Home()
	b.SignInWithGoogle(ada)
	if r := b.HomeSecondStep(codes[0]); r.Status != http.StatusSeeOther {
		t.Fatalf("a recovery code: %d %s", r.Status, r.Text())
	}
	c := w.Home()
	c.SignInWithGoogle(ada)
	if r := c.HomeSecondStep(codes[0]); r.Status == http.StatusSeeOther {
		t.Error("the same recovery code worked twice")
	}
}

// Setting up the app signs the person out everywhere else: those sign-ins
// skipped the step they now need.
func TestSettingUpTheHomeAuthenticatorSignsOutEverywhereElse(t *testing.T) {
	w, _ := homeTwoStepWorld(t)
	elsewhere := w.Home()
	elsewhere.SignInWithGoogle(ada)
	b := w.Home()
	b.SignInWithGoogle(ada)

	b.SetUpHomeTwoStep()

	if r := elsewhere.Get("/account/two-step"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("the other sign-in: %d to %q, want signed out", r.Status, r.Header.Get("Location"))
	}
}

func TestHomeTwoStepIsKeptInPostgres(t *testing.T) {
	w := NewWorldWithPostgres(t, requireHomeTwoStep)
	first := w.Home()
	first.SignInWithGoogle(ada)
	secret, _ := first.SetUpHomeTwoStep()
	if w.DatabaseHolds(t, secret) {
		t.Error("the app's secret is in the database as it is")
	}

	w.RestartPlatform()
	b := w.Home()
	b.SignInWithGoogle(ada)
	w.Clock.Advance(30 * time.Second)
	if r := b.HomeSecondStep(TOTP(t, secret, w.Clock.Now())); r.Status != http.StatusSeeOther {
		t.Errorf("after a restart: %d %s", r.Status, r.Text())
	}
}
