// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Two-step sign-in to a network's console: after the password, a code from
// an authenticator app (or a one-use recovery code). Anyone may turn it on;
// the owner and anyone who may change things across the whole org must,
// where the platform requires it. Once it's on, every way in asks for it.

// Someone already signed in isn't asked for a code: a second step left over
// from signing in again (in another tab, say) takes them to the console,
// and is forgotten. A password alone still gets no further than the code.
func TestSomeoneSignedInIsntAskedForACode(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	cy.Post("/signout", nil)
	cy.PasswordOnly("cy")
	w.Clock.Advance(30 * time.Second)
	cy.SecondStep(TOTP(t, secret, w.Clock.Now()))
	cy.Get("/")
	cy.PasswordOnly("cy") // signing in again, while still signed in

	for _, path := range []string{"/signin/two-step", "/signin"} {
		if r := cy.Get(path); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
			t.Errorf("%s, signed in: %d to %q, want the console", path, r.Status, r.Header.Get("Location"))
		}
	}
	if r := cy.Post("/signin/two-step", url.Values{"code": {"000000"}}); r.Status == http.StatusUnauthorized {
		t.Error("the leftover second step should be forgotten, not asking for a code")
	}

	stranger := w.Browser("acme")
	stranger.PasswordOnly("cy")
	if r := stranger.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("a password alone: %d to %q, want still signed out", r.Status, r.Header.Get("Location"))
	}
}

// The owner opening their network from the home page, while already signed
// in to its console there, goes straight in; anywhere they aren't, the
// second step is still asked for.
func TestOpeningANetworkYoureSignedInToSkipsTheCode(t *testing.T) {
	w, _ := MailWorld(t)
	home, console := w.Created("ada@example.com", "initech")
	console.SetUpTwoStep()
	open := func() string {
		home.Get("/networks/initech")
		u, _ := url.Parse(home.Post("/networks/initech/open", nil).Header.Get("Location"))
		return u.RequestURI()
	}

	if r := console.Get(open()); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Errorf("opening it, signed in there: %d to %q, want straight in", r.Status, r.Header.Get("Location"))
	}
	if r := w.Browser("initech").Handoff(open()); r.Header.Get("Location") != "/signin/two-step" {
		t.Errorf("opening it in another browser: %d to %q, want the second step", r.Status, r.Header.Get("Location"))
	}
}

func TestWithTwoStepOnTheConsoleAsksForACodeAfterThePassword(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	cy.Post("/signout", nil)

	cy.PasswordOnly("cy")

	if r := cy.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("between the steps, the console: %d %q, want still signed out", r.Status, r.Header.Get("Location"))
	}
	if r := cy.SecondStep("000000"); r.Status != http.StatusUnauthorized || !strings.Contains(r.Text(), "didn't match") {
		t.Errorf("a wrong code: %d %s", r.Status, r.Text())
	}
	w.Clock.Advance(30 * time.Second) // a code the app shows now, not the one used to set it up
	if r := cy.SecondStep(TOTP(t, secret, w.Clock.Now())); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("the right code: %d %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if r := cy.Get("/"); r.Status != http.StatusOK {
		t.Errorf("signed in: %d", r.Status)
	}
}

func TestAnAuthenticatorCodeWorksOnce(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	w.Clock.Advance(30 * time.Second)
	code := TOTP(t, secret, w.Clock.Now())
	first := w.Browser("acme")
	first.PasswordOnly("cy")
	if r := first.SecondStep(code); r.Status != http.StatusSeeOther {
		t.Fatalf("the code, first time: %d %s", r.Status, r.Text())
	}

	again := w.Browser("acme")
	again.PasswordOnly("cy")
	if r := again.SecondStep(code); r.Status != http.StatusUnauthorized {
		t.Errorf("the same code again: %d, want refused", r.Status)
	}
}

func TestSettingUpTwoStepNeedsACodeFromTheApp(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	page := cy.Get("/account/two-step")
	secret := SecretField.FindStringSubmatch(page.Body)[1]
	if !strings.Contains(page.Body, "otpauth://totp/") && !strings.Contains(page.Body, "data:image/png;base64,") {
		t.Error("the setup page should show a QR code for the app")
	}

	r := cy.Post("/account/two-step", url.Values{"secret": {secret}, "code": {"123456"}})

	if r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "didn't match") {
		t.Errorf("a wrong code: %d %s", r.Status, r.Text())
	}
	cy.Post("/signout", nil)
	if r := w.Browser("acme").SignIn("cy", "cy-pw"); r.Header.Get("Location") != "/" {
		t.Errorf("after a failed setup, cy's password alone should still sign in: %q", r.Header.Get("Location"))
	}
}

func TestARecoveryCodeSignsInOnce(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	_, codes := cy.SetUpTwoStep()
	b := w.Browser("acme")
	b.PasswordOnly("cy")

	if r := b.SecondStep(strings.ToUpper(strings.ReplaceAll(codes[3], "-", " "))); r.Status != http.StatusSeeOther {
		t.Fatalf("a recovery code, typed loosely: %d %s", r.Status, r.Text())
	}

	again := w.Browser("acme")
	again.PasswordOnly("cy")
	if r := again.SecondStep(codes[3]); r.Status != http.StatusUnauthorized {
		t.Errorf("the same recovery code again: %d", r.Status)
	}
	if page := b.Get("/account"); !strings.Contains(page.Text(), "9 recovery codes left") {
		t.Errorf("the account page should say how many are left:\n%s", page.Text())
	}
}

func TestTooManyWrongCodesMeansStartingAgain(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	b := w.Browser("acme")
	b.PasswordOnly("cy")
	for range 5 {
		b.SecondStep("000000")
	}
	w.Clock.Advance(30 * time.Second)

	r := b.SecondStep(TOTP(t, secret, w.Clock.Now()))

	if r.Status == http.StatusSeeOther && r.Header.Get("Location") == "/" {
		t.Fatal("after five wrong codes, a right one still signed in")
	}
	if !strings.Contains(r.Text(), "Sign in again") {
		t.Errorf("they should be told to start again:\n%s", r.Text())
	}
}

// Someone with the password, guessing codes from many sign-ins at once, gets
// no more guesses than one person typing them would.
func TestCodesSentAtOnceAreStillLimited(t *testing.T) {
	worlds := map[string]func(*testing.T) *World{
		"memory":   NewWorld,
		"postgres": func(t *testing.T) *World { return NewWorldWithPostgres(t) },
	}
	for name, world := range worlds {
		t.Run(name, func(t *testing.T) {
			w := world(t)
			cy := w.Browser("acme").SignedIn("cy")
			cy.SetUpTwoStep()
			var guessers []*Browser
			for range 10 {
				b := w.Browser("acme")
				b.PasswordOnly("cy")
				for range 10 {
					g := w.Browser("acme")
					g.Cookies = maps.Clone(b.Cookies)
					guessers = append(guessers, g)
				}
			}
			var checked atomic.Int64
			var wg sync.WaitGroup
			for _, g := range guessers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if r := g.Post("/signin/two-step", url.Values{"code": {"000000"}}); r.Status == http.StatusUnauthorized {
						checked.Add(1)
					}
				}()
			}
			wg.Wait()
			if n := checked.Load(); n > 10 {
				t.Errorf("%d of %d codes sent at once were checked; the limit is 10", n, len(guessers))
			}
		})
	}
}

func TestTurningOnTwoStepSignsYouOutEverywhereElse(t *testing.T) {
	w := NewWorld(t)
	laptop := w.Browser("acme").SignedIn("cy")
	phone := w.Browser("acme").SignedIn("cy")

	phone.SetUpTwoStep()

	if r := laptop.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("the other sign-in: %d, want signed out", r.Status)
	}
	if r := phone.Get("/"); r.Status != http.StatusOK {
		t.Errorf("where it was turned on: %d, want still signed in", r.Status)
	}
}

func TestTurningTwoStepOffNeedsACode(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, _ := cy.SetUpTwoStep()
	cy.Get("/account")

	if r := cy.Post("/account/two-step/off", url.Values{"code": {"000000"}}); r.Status != http.StatusUnauthorized {
		t.Errorf("off with a wrong code: %d", r.Status)
	}
	w.Clock.Advance(30 * time.Second)
	cy.Get("/account")
	if r := cy.Post("/account/two-step/off", url.Values{"code": {TOTP(t, secret, w.Clock.Now())}}); r.Status != http.StatusSeeOther {
		t.Fatalf("off with the right code: %d %s", r.Status, r.Text())
	}

	if r := w.Browser("acme").SignIn("cy", "cy-pw"); r.Header.Get("Location") != "/" {
		t.Errorf("with it off, the password alone signs in: %q", r.Header.Get("Location"))
	}
}

func TestTheOwnerAndOrgWideAdminsMustSetUpTwoStep(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, RequireTwoStep)
	ada := w.Browser("acme").SignedIn("ada")

	if r := ada.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
		t.Errorf("the owner without it: %d %q, want sent to set it up", r.Status, r.Header.Get("Location"))
	}
	ada.Get("/account/two-step")
	if r := ada.Post("/staff", url.Values{"nick": {"sam"}, "role": {"Engineer"}}); r.Status != http.StatusForbidden {
		t.Errorf("hiring without it: %d, want refused", r.Status)
	}
	if r := ada.Post("/chat/token", nil); r.Status != http.StatusForbidden {
		t.Errorf("web chat without it: %d, want refused", r.Status)
	}
	for _, path := range []string{"/chat/app/config.json", "/chat/app/approvals"} {
		if r := ada.Get(path); r.Status != http.StatusForbidden {
			t.Errorf("%s without it: %d, want refused", path, r.Status)
		}
	}

	ada.SetUpTwoStep()
	if r := ada.Get("/"); r.Status != http.StatusOK {
		t.Errorf("with it set up: %d", r.Status)
	}

	// cy may only work in their own team, so needn't; given Admin, cy must.
	cy := w.Browser("acme").SignedIn("cy")
	if r := cy.Get("/"); r.Status != http.StatusOK {
		t.Errorf("cy, with no org-wide abilities: %d, want in", r.Status)
	}
	ada.Get("/people/cy")
	if r := ada.Post("/people/cy/access", url.Values{"level": {"Admin"}, "give": {"1"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("giving cy Admin: %d %s", r.Status, r.Text())
	}
	if r := cy.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
		t.Errorf("cy, now an admin: %d %q, want sent to set it up", r.Status, r.Header.Get("Location"))
	}
}

// Leading the team every other team is inside is as good as changing the
// whole org: that lead must set up two-step too (as the runner says).
func TestTheLeadOfEveryTeamMustSetUpTwoStep(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, RequireTwoStep)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/account/two-step")
	ada.SetUpTwoStep()
	cy := w.Browser("acme").SignedIn("cy")
	if r := cy.Get("/"); r.Status != http.StatusOK {
		t.Fatalf("cy, leading nothing: %d, want in", r.Status)
	}

	ada.Get("/teams")
	if r := ada.Post("/teams", url.Values{"name": {"Company"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("making Company: %d %s", r.Status, r.Text())
	}
	for team, form := range map[string]url.Values{"Company": {"leads": {"cy"}}, "Engineering": {"parent": {"Company"}}, "Sales": {"parent": {"Company"}}} {
		ada.Get("/teams/" + team)
		if r := ada.Post("/teams/"+team, form); r.Status != http.StatusSeeOther {
			t.Fatalf("setting %s %v: %d %s", team, form, r.Status, r.Text())
		}
	}

	if r := cy.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/account/two-step" {
		t.Errorf("cy, leading the team every team is inside: %d %q, want sent to set it up", r.Status, r.Header.Get("Location"))
	}
}

func TestOpeningTheConsoleFromTheHomePageStillAsksTheSecondStep(t *testing.T) {
	w := NewWorld(t)
	home, console := w.Created("ada@example.com", "initech")
	console.SetUpTwoStep()

	home.Get("/networks/initech")
	to, _ := url.Parse(home.Post("/networks/initech/open", nil).Header.Get("Location"))
	b := w.Browser("initech")
	r := b.Handoff(to.RequestURI())

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin/two-step" {
		t.Errorf("opening it: %d %q, want the second step", r.Status, r.Header.Get("Location"))
	}
	if r := b.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("before the second step: %d, want not signed in", r.Status)
	}
}

func TestAResetLinkStillAsksTheSecondStep(t *testing.T) {
	w := NewWorld(t)
	w.Browser("acme").SignedIn("cy").SetUpTwoStep()
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people")
	link := resetLinkIn(t, ada.Post("/people/cy/reset", nil).Body)
	cy := w.Browser("acme")
	cy.Get(link)

	r := cy.Post(link, url.Values{"password": {"a-new-long-password"}, "confirm": {"a-new-long-password"}})

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin/two-step" {
		t.Errorf("after the reset: %d %q, want the second step", r.Status, r.Header.Get("Location"))
	}
}

var resetLinkPath = regexp.MustCompile(`/reset/[A-Za-z0-9_-]+`)

func resetLinkIn(t *testing.T, body string) string {
	t.Helper()
	link := resetLinkPath.FindString(body)
	if link == "" {
		t.Fatalf("no reset link on the page:\n%s", body)
	}
	return link
}

func TestTheOwnerCanResetSomeonesTwoStep(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	cy.SetUpTwoStep()
	bo := w.Browser("acme").SignedIn("bo")
	bo.Get("/people/cy")
	if r := bo.Post("/people/cy/two-step/reset", nil); r.Status != http.StatusForbidden {
		t.Errorf("bo resetting cy's: %d, want refused", r.Status)
	}
	ada := w.Browser("acme").SignedIn("ada")
	page := ada.Get("/people/cy")
	if !strings.Contains(page.Text(), "Two-step sign-in is on") {
		t.Errorf("the owner should see cy has it on:\n%s", page.Text())
	}

	if r := ada.Post("/people/cy/two-step/reset", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("ada resetting cy's: %d %s", r.Status, r.Text())
	}

	if r := cy.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("cy's sign-in after the reset: %d, want signed out", r.Status)
	}
	if r := w.Browser("acme").SignIn("cy", "cy-pw"); r.Header.Get("Location") != "/" {
		t.Errorf("cy's password alone after the reset: %q", r.Header.Get("Location"))
	}
}

func TestTheOwnerResetsTheirOwnTwoStepFromTheHomePage(t *testing.T) {
	w, out := MailWorld(t)
	home, console := w.Created("ada@example.com", "initech")
	console.SetUpTwoStep()
	page := home.Get("/networks/initech")
	if !strings.Contains(page.Text(), "Two-step sign-in is on") {
		t.Errorf("the network's page should say two-step sign-in is on:\n%s", page.Text())
	}

	if r := home.Post("/networks/initech/two-step/reset", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("resetting it: %d %s", r.Status, r.Text())
	}

	if r := console.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("the console after the reset: %d, want signed out", r.Status)
	}
	if r := w.Browser("initech").SignIn("ada", "correct-horse-battery"); r.Header.Get("Location") != "/" {
		t.Errorf("the password alone after the reset: %q", r.Header.Get("Location"))
	}
	out.One(t, "ada@example.com", "Two-step sign-in for ada on initech was reset", "ada", "initech", "If that wasn't you")
}

func TestTheOwnerHearsWhenARecoveryCodeIsUsed(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	_, codes := console.SetUpTwoStep()
	b := w.Browser("initech")
	b.SignIn("ada", "correct-horse-battery")

	b.SecondStep(codes[0])

	m := out.One(t, "ada@example.com", "recovery code", "ada", "9")
	if strings.Contains(m.Text+m.HTML, codes[0]) || strings.Contains(m.Text+m.HTML, codes[1]) {
		t.Error("the email carries a recovery code")
	}
}

func TestTwoStepCodesAreKeptOneUseInEitherStore(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		steps := s.(platform.TwoSteps)
		if err := s.AddNetwork(ctx, acmeNet); err != nil {
			t.Fatal(err)
		}
		if got, _ := steps.TwoStepOf(ctx, "acme", "cy"); got.On() {
			t.Fatal("on before it was set up")
		}
		setup := platform.TwoStepSetup{Secret: "JBSWY3DPEHPK3PXP", LastStep: 100, Recovery: []string{"h1", "h2"}, Since: time.Unix(1_700_000_000, 0).UTC()}
		if err := steps.SetTwoStep(ctx, "acme", "Cy", setup); err != nil {
			t.Fatal(err)
		}

		for _, c := range []struct {
			step int64
			want bool
		}{{100, false}, {101, true}, {101, false}, {99, false}, {103, true}} {
			if ok, err := steps.UseStep(ctx, "acme", "cy", c.step); ok != c.want || err != nil {
				t.Errorf("step %d: %v %v, want %v", c.step, ok, err, c.want)
			}
		}
		if left, ok, _ := steps.UseRecovery(ctx, "acme", "cy", "h2"); !ok || left != 1 {
			t.Errorf("a recovery code: %v, %d left", ok, left)
		}
		if _, ok, _ := steps.UseRecovery(ctx, "acme", "cy", "h2"); ok {
			t.Error("the same recovery code twice")
		}
		got, _ := steps.TwoStepOf(ctx, "ACME", "cy")
		if got.Secret != setup.Secret || got.LastStep != 103 || len(got.Recovery) != 1 || !got.Since.Equal(setup.Since) {
			t.Errorf("kept: %+v", got)
		}
		steps.RemoveTwoStep(ctx, "acme", "cy")
		if got, _ := steps.TwoStepOf(ctx, "acme", "cy"); got.On() {
			t.Error("still on after removing it")
		}
	})
}

func TestTwoStepSecretsAreKeptSealed(t *testing.T) {
	w := NewWorldWithPostgres(t)
	cy := w.Browser("acme").SignedIn("cy")
	secret, codes := cy.SetUpTwoStep()

	for _, s := range append([]string{secret}, codes...) {
		if w.DatabaseHolds(t, s) {
			t.Errorf("the database holds %q as it is", s)
		}
	}
	w.RestartPlatform()
	w.Clock.Advance(30 * time.Second)
	b := w.Browser("acme")
	b.PasswordOnly("cy")
	if r := b.SecondStep(TOTP(t, secret, w.Clock.Now())); r.Status != http.StatusSeeOther {
		t.Errorf("after a restart, the app's code: %d %s", r.Status, r.Text())
	}
}
