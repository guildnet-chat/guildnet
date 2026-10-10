// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// First-run setup of a self-hosted install, which runs one network (hq),
// already named at install. Until it's set up, the console prints a setup
// code to its log and the home page asks for it: whoever has it (whoever
// can read the server's log) sets the network up, choosing its template,
// timezone, owner and model key, and is handed straight into its console as
// the owner. Once it's set up there's no code and no setup, and the home
// page sends people to the console.

func TestBeforeSetupTheHomePageAsksForTheSetupCode(t *testing.T) {
	w := NewSelfHostedWorld(t)

	r := w.Home().Get("/")

	if r.Status != http.StatusOK || !strings.Contains(r.Text(), "setup code") || !strings.Contains(r.Body, `action="/setup"`) {
		t.Fatalf("the home page: %d\n%s", r.Status, r.Text())
	}
	if !strings.Contains(r.Text(), "hq."+BaseDomain) {
		t.Errorf("the page should say which network it sets up:\n%s", r.Text())
	}
	if len(w.SetupCodes()) != 1 {
		t.Errorf("the console's log should have one setup code:\n%s", w.Logged())
	}
	if r := w.Browser(SelfHosted).Get("/signin"); r.Status != http.StatusNotFound {
		t.Errorf("hq's console before setup: %d", r.Status)
	}
}

func TestTheRunnerWaitsForSetup(t *testing.T) {
	w := NewSelfHostedWorld(t)

	time.Sleep(200 * time.Millisecond)

	if w.Stack.Started() {
		t.Fatal("the runner started before setup")
	}
	if status, out := BrokerCall(t, w, SelfHosted, SelfHostedRunnerToken, "GET", "/settings"); status != http.StatusNotFound || !strings.Contains(fmt.Sprint(out["error"]), "set up") {
		t.Errorf("the broker before setup: %d %v", status, out)
	}
}

func TestSettingUpMakesTheNetworkAndHandsTheOwnerIn(t *testing.T) {
	w := NewSelfHostedWorld(t)

	_, console := w.SetUp()

	if r := console.Get("/"); r.Status != http.StatusOK || !strings.Contains(r.Text(), "ada") {
		t.Fatalf("ada's console: %d %s", r.Status, r.Text())
	}
	if pw, ok := w.Ergo.Password(SelfHosted, "ada"); !ok || pw != "correct-horse-battery" {
		t.Errorf("ada's IRC account: %q %v", pw, ok)
	}
	if got := w.Stack.Settings(); got != (NetworkSettings{Template: "team", Owner: "ada", Company: SelfHosted, Timezone: "Europe/Paris"}) {
		t.Errorf("the runner was told %+v", got)
	}
	if status, out := BrokerCall(t, w, SelfHosted, SelfHostedRunnerToken, "GET", "/model-keys"); status != http.StatusOK ||
		!strings.Contains(fmt.Sprint(out["keys"]), "test-key") {
		t.Errorf("the runner's keys: %d %v", status, out)
	}
	n, err := w.Store.Network(context.Background(), SelfHosted)
	if err != nil || n.Status != platform.Ready || n.Owner != "" || n.OwnerNick != "ada" {
		t.Errorf("hq: %+v %v", n, err)
	}
	if r := console.Get("/settings"); r.Status != http.StatusOK {
		t.Errorf("ada's settings: %d", r.Status)
	}
}

func TestAWrongSetupCodeSetsUpNothing(t *testing.T) {
	w := NewSelfHostedWorld(t)
	b := w.Home()
	b.Get("/")

	r := b.Post("/setup", SetupForm("AAAA-BBBB-CCCC"))

	if r.Status != http.StatusForbidden || !strings.Contains(r.Text(), "setup code") {
		t.Errorf("a wrong code: %d %s", r.Status, r.Text())
	}
	if _, err := w.Store.Network(context.Background(), SelfHosted); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("hq exists: %v", err)
	}
	if !strings.Contains(w.Logged(), "wrong setup code") {
		t.Errorf("the log should say a wrong code was tried:\n%s", w.Logged())
	}
}

// Guessing is slowed down: after a few wrong codes from an address, even
// the right one is refused there for a while.
func TestGuessingTheSetupCodeIsSlowedDown(t *testing.T) {
	w := NewSelfHostedWorld(t)
	b := w.Home()
	b.Get("/")
	for range 5 {
		b.Post("/setup", SetupForm("AAAA-BBBB-CCCC"))
	}

	if r := b.Post("/setup", SetupForm(w.SetupCode())); r.Status != http.StatusTooManyRequests {
		t.Errorf("the right code after five wrong ones: %d", r.Status)
	}
	w.Clock.Advance(16 * time.Minute)
	if r := b.Post("/setup", SetupForm(w.SetupCode())); r.Status != http.StatusSeeOther {
		t.Errorf("the right code later: %d %s", r.Status, r.Text())
	}
}

func TestOnceSetUpThereIsNoSetup(t *testing.T) {
	w := NewSelfHostedWorld(t)
	code := w.SetupCode()
	w.SetUp()
	someone := w.Home()

	if r := someone.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "http://hq."+BaseDomain+"/" {
		t.Errorf("the home page once set up: %d to %q", r.Status, r.Header.Get("Location"))
	}
	form := SetupForm(code)
	form.Set("nick", "mallory")
	if r := someone.Post("/setup", form); r.Status == http.StatusSeeOther {
		t.Error("the code worked a second time")
	}
	if n, _ := w.Store.Network(context.Background(), SelfHosted); n.OwnerNick != "ada" {
		t.Errorf("hq's owner is %q", n.OwnerNick)
	}
	w.RestartPlatform()
	if len(w.SetupCodes()) != 1 {
		t.Errorf("a restart once set up printed a code:\n%s", w.Logged())
	}
}

func TestARestartBeforeSetupPrintsANewCode(t *testing.T) {
	w := NewSelfHostedWorld(t)
	old := w.SetupCode()

	w.RestartPlatform()

	if w.SetupCode() == old {
		t.Fatal("the code didn't change")
	}
	b := w.Home()
	b.Get("/")
	if r := b.Post("/setup", SetupForm(old)); r.Status != http.StatusForbidden {
		t.Errorf("the old code: %d", r.Status)
	}
	if r := b.Post("/setup", SetupForm(w.SetupCode())); r.Status != http.StatusSeeOther {
		t.Errorf("the new code: %d %s", r.Status, r.Text())
	}
}

// An install moved (from Compose to a cloud, say) finds its runner where its
// settings now say: the console takes the network's addresses from them at
// every start, not only at setup.
func TestAMovedInstallFindsItsRunnerWhereItsSettingsSay(t *testing.T) {
	w := NewSelfHostedWorld(t)
	_, console := w.SetUp()
	if r := console.Get("/people"); r.Status != http.StatusOK || !strings.Contains(r.Text(), "ada") {
		t.Fatalf("hq's people before the move: %d %s", r.Status, r.Text())
	}

	w.Stack.Move()
	w.RestartPlatform()

	if r := console.Get("/people"); r.Status != http.StatusOK || !strings.Contains(r.Text(), "ada") {
		t.Errorf("hq's people after the move: %d %s", r.Status, r.Text())
	}
	if !strings.Contains(w.Logged(), "addresses") {
		t.Errorf("the console should log that hq's addresses changed:\n%s", w.Logged())
	}
}

// Only the browser that entered the code sees setting up and is handed in.
func TestOnlyTheBrowserThatSetItUpIsHandedIn(t *testing.T) {
	w := NewSelfHostedWorld(t)
	w.SetUp()
	someone := w.Home()
	someone.Get("/")

	if r := someone.Post("/setup/open", nil); r.Status == http.StatusSeeOther && strings.Contains(r.Header.Get("Location"), "/welcome") {
		t.Errorf("someone else was handed in: %q", r.Header.Get("Location"))
	}
}

// Browsers hold a form's redirect to the policy of the page the form is on,
// and "Open" arrives (by itself, as setting up finishes) on a page that
// loaded before: whatever it shows, the page lets its forms reach the
// console.
func TestSetupsPageLetsItsFormsReachTheConsoleWhateverItShows(t *testing.T) {
	w := NewSelfHostedWorld(t)
	b := w.Home()
	want := "form-action 'self' http://hq." + BaseDomain
	if csp := b.Get("/").Header.Get("Content-Security-Policy"); !strings.Contains(csp, want) {
		t.Errorf("before setting up: %q, want %s", csp, want)
	}
	b.Post("/setup", SetupForm(w.SetupCode()))
	if csp := b.Get("/").Header.Get("Content-Security-Policy"); !strings.Contains(csp, want) {
		t.Errorf("setting up: %q, want %s", csp, want)
	}
}

// Setting up that doesn't finish can be tried again, with the code.
func TestSetupThatFailsCanBeTriedAgain(t *testing.T) {
	w := NewSelfHostedWorld(t)
	w.Ergo.AddAccount(SelfHosted, "ada", "someone-elses-password")
	b := w.Home()
	b.Get("/")
	b.Post("/setup", SetupForm(w.SetupCode()))

	r := b.WaitSetUp()

	if !strings.Contains(r.Text(), "different password") || !strings.Contains(r.Body, `action="/setup"`) {
		t.Fatalf("after failing: %d %s", r.Status, r.Text())
	}
	form := SetupForm(w.SetupCode())
	form.Set("nick", "bea")
	if r := b.Post("/setup", form); r.Status != http.StatusSeeOther {
		t.Fatalf("trying again: %d %s", r.Status, r.Text())
	}
	if r := b.WaitSetUp(); !strings.Contains(r.Text(), "is ready") {
		t.Fatalf("after trying again: %s", r.Text())
	}
	if got := w.Stack.Settings(); got.Owner != "bea" {
		t.Errorf("the runner was told %+v", got)
	}
}

func TestTheSetupFormIsChecked(t *testing.T) {
	w := NewSelfHostedWorld(t)
	b := w.Home()
	b.Get("/")
	for what, change := range map[string][2]string{
		"different": {"confirm", "another-long-password"},
		"nick":      {"nick", "1ada"},
		"timezone":  {"timezone", "Mars/Olympus"},
		"template":  {"template", "nope"},
	} {
		form := SetupForm(w.SetupCode())
		form.Set(change[0], change[1])
		if r := b.Post("/setup", form); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), what) {
			t.Errorf("%s=%s: %d %s", change[0], change[1], r.Status, r.Text())
		}
	}
	if _, err := w.Store.Network(context.Background(), SelfHosted); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("hq exists: %v", err)
	}
}

// A self-hosted install runs only its one network: none can be created on
// the home page, even by someone who can sign in there.
func TestASelfHostedInstallMakesNoOtherNetwork(t *testing.T) {
	w := NewSelfHostedWorld(t, func(o *Setup) { o.DevSignIn = true })
	w.SetUp()
	b := w.Home()
	b.Get("/signin")
	b.Post("/signin/dev", url.Values{"email": {"eve@example.com"}})

	if r := b.Post("/networks", CreateForm("other")); r.Status == http.StatusSeeOther {
		t.Errorf("eve created a network: %d %s", r.Status, r.Header.Get("Location"))
	}
	if _, err := w.Store.Network(context.Background(), "other"); err == nil {
		t.Error("there's a second network")
	}
}

// Settings change the runner's settings; the network isn't deleted from
// the console (its stack is the install's).
func TestASelfHostedNetworksSettings(t *testing.T) {
	w := NewSelfHostedWorld(t)
	_, console := w.SetUp()
	console.Get("/settings")

	if r := console.Post("/settings", url.Values{"timezone": {"Asia/Tokyo"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("changing the timezone: %d %s", r.Status, r.Text())
	}
	WaitFor(t, "the runner to hear the new timezone", func() bool { return w.Stack.Settings().Timezone == "Asia/Tokyo" })

	page := console.Get("/settings")
	if strings.Contains(page.Body, `action="/settings/delete"`) {
		t.Errorf("the settings offer deleting it:\n%s", page.Text())
	}
	if r := console.Post("/settings/delete", url.Values{"confirm": {SelfHosted}}); r.Status != http.StatusBadRequest {
		t.Errorf("deleting it: %d %s", r.Status, r.Text())
	}
	if n, err := w.Store.Network(context.Background(), SelfHosted); err != nil || n.Status != platform.Ready {
		t.Errorf("hq after trying to delete it: %+v %v", n, err)
	}
}

// With email, the owner can give an address for alerts at setup: it's
// confirmed by a link before anything goes there.
func TestTheOwnerCanGiveAnAlertAddressAtSetup(t *testing.T) {
	out := &Outbox{}
	w := NewSelfHostedWorld(t, WithMail(out))
	b := w.Home()
	if !strings.Contains(b.Get("/").Body, `name="email"`) {
		t.Fatal("the setup page should ask for an address when it can send email")
	}
	form := SetupForm(w.SetupCode())
	form.Set("email", "ada@hq.example")

	b.Post("/setup", form)
	b.WaitSetUp()

	m := out.One(t, "ada@hq.example", "Confirm your email", "hq")
	if !strings.Contains(m.Text, "http://hq."+BaseDomain+"/account/email/confirm/") {
		t.Errorf("the link: %s", m.Text)
	}
}
