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

// A self-hosted network's owner who's lost their password and their second
// step has no home page to reset from: whoever runs the server makes them a
// link from its command line (reset-owner). Used once, within the hour, it
// sets a new password and turns their two-step sign-in off until they set
// it up again. Having the server already means having everything, so it
// opens no new way in.

func resetOwner(t *testing.T, w *World) (string, error) {
	t.Helper()
	return platform.ResetOwner(context.Background(), platform.Options{BaseDomain: BaseDomain, Networks: w.Store, Links: w.Store,
		FirstRun: &platform.FirstRun{Network: SelfHosted}, Now: w.Clock.Now})
}

func TestALockedOutOwnerIsResetFromTheServer(t *testing.T) {
	w := NewSelfHostedWorld(t)
	_, ada := w.SetUp()
	ada.Get("/account/two-step")
	ada.SetUpTwoStep()

	link, err := resetOwner(t, w)

	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Host != SelfHosted+"."+BaseDomain || !strings.HasPrefix(u.Path, "/reset/") {
		t.Fatalf("the link: %q", link)
	}
	b := w.Browser(SelfHosted)
	if r := b.Get(u.Path); r.Status != http.StatusOK {
		t.Fatalf("the reset page: %d %s", r.Status, r.Text())
	}
	r := b.Post(u.Path, url.Values{"password": {"a-brand-new-password"}, "confirm": {"a-brand-new-password"}})
	if r.Status != http.StatusSeeOther || strings.Contains(r.Header.Get("Location"), "/signin") {
		t.Fatalf("setting a new password: %d to %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if page := b.Get("/"); page.Status != http.StatusOK || !strings.Contains(page.Text(), "ada") {
		t.Errorf("ada's console after the reset: %d %s", page.Status, page.Text())
	}
	if !SignsInOverIRC(t, w, SelfHosted, "ada", "a-brand-new-password") {
		t.Error("ada's new IRC password doesn't sign in")
	}
	if ts, err := w.Store.TwoStepOf(context.Background(), SelfHosted, "ada"); err != nil || ts.On() {
		t.Errorf("ada's two-step sign-in is still on (%v)", err)
	}
	if r := ada.Get("/"); r.Status == http.StatusOK {
		t.Error("ada's old sign-in still works")
	}
	if r := w.Browser(SelfHosted).Get(u.Path); r.Status != http.StatusGone {
		t.Errorf("the link a second time: %d", r.Status)
	}
}

func TestAnOwnerResetLinkLastsAnHour(t *testing.T) {
	w := NewSelfHostedWorld(t)
	w.SetUp()
	link, _ := resetOwner(t, w)
	w.Clock.Advance(61 * time.Minute)
	u, _ := url.Parse(link)

	if r := w.Browser(SelfHosted).Get(u.Path); r.Status != http.StatusGone {
		t.Errorf("after an hour: %d", r.Status)
	}
}

func TestThereIsNoOwnerToResetBeforeSetup(t *testing.T) {
	w := NewSelfHostedWorld(t)

	if _, err := resetOwner(t, w); err == nil || !strings.Contains(err.Error(), "set up") {
		t.Errorf("before setup: %v", err)
	}
}

// Someone with only the owner's password can't use an ordinary reset link
// to get round their second step: only the server makes the owner's.
func TestTheConsoleNeverMakesTheOwnerAResetLink(t *testing.T) {
	w := NewSelfHostedWorld(t)
	_, ada := w.SetUp()
	ada.Get("/people")

	if r := ada.Post("/people/ada/reset", nil); r.Status != http.StatusForbidden {
		t.Errorf("a reset link for the owner, from the console: %d", r.Status)
	}
}
