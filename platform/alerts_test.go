// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Email alerts for people in a network. Anyone can give an address on their
// account page, confirmed by a link sent there before anything else goes to
// it (the owner's is their Google address, already theirs). Then they can
// ask to be emailed when an approval is waiting on them: off until they
// turn it on. Networks' runners tell the platform what happened, through the
// broker, with the network's secret.

var confirmLink = regexp.MustCompile(`/account/email/confirm/[A-Za-z0-9_-]{20,}`)

// news posts a runner's event for network to the broker, as its runner.
func news(t *testing.T, w *World, network, secret, body string) int {
	t.Helper()
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	req, _ := http.NewRequest("POST", broker.URL+"/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("GuildNet-Network", network)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func secretOf(t *testing.T, w *World, network string) string {
	n, err := w.Store.Network(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	return n.RunnerToken
}

func approvalFor(approver string) string {
	return `{"type":"approval","approval":{"id":3,"agent":"kai","summary":"merge PR #17 into main","where":"#engineering","asker":"ada","approver":"` +
		approver + `","expires":"2026-10-01T16:00:00Z"}}`
}

// deeJoins has dee join initech with an invite, signed in to its console.
func deeJoins(t *testing.T, w *World, console *Browser) *Browser {
	t.Helper()
	console.Get("/people")
	link := JoinLink.FindString(console.Post("/people/invite", url.Values{"role": {"Engineer"}}).Body)
	dee := w.Browser("initech")
	dee.Get(link)
	if r := dee.Post(link, url.Values{"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("dee joining: %d %s", r.Status, r.Text())
	}
	return dee
}

func TestSomeoneGivesAnAddressAndConfirmsIt(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	dee := deeJoins(t, w, console)
	dee.Get("/account")

	r := dee.Post("/account/alerts/email", url.Values{"email": {" Dee@Example.com "}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("giving an address: %d %s", r.Status, r.Text())
	}
	m := out.One(t, "dee@example.com", "Confirm your email", "initech")
	link := confirmLink.FindString(m.Text)
	if link == "" || !strings.Contains(m.HTML, link) {
		t.Fatalf("the email should carry a confirmation link:\n%s", m.Text)
	}
	if page := VisibleText(dee.Get("/account")); !strings.Contains(page, "waiting for you to confirm") {
		t.Errorf("the account page should say the address isn't confirmed yet:\n%s", page)
	}

	// Opened anywhere (another browser, a phone), it confirms the address.
	if r := w.Browser("initech").Get(link); r.Status != http.StatusOK || !strings.Contains(r.Text(), "confirmed") {
		t.Errorf("confirming: %d %s", r.Status, r.Text())
	}
	if page := VisibleText(dee.Get("/account")); !strings.Contains(page, "dee@example.com") || strings.Contains(page, "waiting for you to confirm") {
		t.Errorf("the account page should show the confirmed address:\n%s", page)
	}
	if r := w.Browser("initech").Get(link); r.Status != http.StatusGone {
		t.Errorf("the link a second time: %d, want 410", r.Status)
	}
}

// Kept in the database: an address given, confirmed, and approval emails
// on, across the platform's restarts.
func TestAlertAddressesAreKeptInTheDatabase(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWithPostgres(t, WithMail(out))
	_, console := w.Created("ada@example.com", "initech")
	dee := deeJoins(t, w, console)
	dee.Get("/account")
	dee.Post("/account/alerts/email", url.Values{"email": {"dee@example.com"}})
	link := confirmLink.FindString(out.One(t, "dee@example.com", "Confirm").Text)
	if r := w.Browser("initech").Get(link); r.Status != http.StatusOK {
		t.Fatalf("confirming: %d %s", r.Status, r.Text())
	}
	dee.Get("/account")
	dee.Post("/account/alerts", url.Values{"approvals": {"on"}})

	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("dee"))

	out.One(t, "dee@example.com", "approval", "merge PR #17 into main")
	if r := w.Browser("initech").Get(link); r.Status != http.StatusGone {
		t.Errorf("the link a second time: %d, want 410", r.Status)
	}

	// And forgotten when dee leaves.
	news(t, w, "initech", secretOf(t, w, "initech"), `{"type":"member_removed","member":{"account":"dee"}}`)
	news(t, w, "initech", secretOf(t, w, "initech"), strings.Replace(approvalFor("dee"), `"id":3`, `"id":4`, 1))
	if !out.None(t, "dee@example.com", "#4") {
		t.Error("dee left, but an approval still went to their address")
	}
}

func TestAConfirmationLinkLastsADay(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	dee := deeJoins(t, w, console)
	dee.Get("/account")
	dee.Post("/account/alerts/email", url.Values{"email": {"dee@example.com"}})
	link := confirmLink.FindString(out.One(t, "dee@example.com", "Confirm").Text)

	w.Clock.Advance(24*time.Hour + time.Minute)

	if r := w.Browser("initech").Get(link); r.Status != http.StatusGone {
		t.Errorf("a day later: %d, want 410", r.Status)
	}
}

func TestApprovalEmailsAreOffUntilSomeoneTurnsThemOn(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	dee := deeJoins(t, w, console)
	dee.Get("/account")
	dee.Post("/account/alerts/email", url.Values{"email": {"dee@example.com"}})
	w.Browser("initech").Get(confirmLink.FindString(out.One(t, "dee@example.com", "Confirm").Text))

	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("dee"))
	if !out.None(t, "dee@example.com", "approval") {
		t.Fatal("dee hasn't turned approval emails on, but got one")
	}

	page := dee.Get("/account")
	if !regexp.MustCompile(`<input type="checkbox" name="approvals"[^>]*>`).MatchString(page.Body) || strings.Contains(regexp.MustCompile(`<input type="checkbox" name="approvals"[^>]*>`).FindString(page.Body), "checked") {
		t.Fatalf("the account page should offer approval emails, off:\n%s", page.Body)
	}
	if r := dee.Post("/account/alerts", url.Values{"approvals": {"on"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("turning them on: %d %s", r.Status, r.Text())
	}
	if status := news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("dee")); status != http.StatusAccepted {
		t.Fatalf("the event: %d", status)
	}

	m := out.One(t, "dee@example.com", "approval", "kai", "merge PR #17 into main", "APPROVE 3", "http://initech."+BaseDomain+"/approvals")
	if !strings.Contains(m.Text, "account page") {
		t.Errorf("the email should say how to turn these off:\n%s", m.Text)
	}

	dee.Get("/account")
	dee.Post("/account/alerts", url.Values{})
	news(t, w, "initech", secretOf(t, w, "initech"), strings.Replace(approvalFor("dee"), `"id":3`, `"id":4`, 1))
	if !out.None(t, "dee@example.com", "#4") {
		t.Error("dee turned approval emails off, but got one")
	}
}

// An outside agent's approval in its own words is marked as its own in the
// email too.
func TestAnApprovalEmailMarksAnAgentsOwnWords(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	dee := deeJoins(t, w, console)
	dee.Get("/account")
	dee.Post("/account/alerts/email", url.Values{"email": {"dee@example.com"}})
	w.Browser("initech").Get(confirmLink.FindString(out.One(t, "dee@example.com", "Confirm").Text))
	dee.Get("/account")
	dee.Post("/account/alerts", url.Values{"approvals": {"on"}})

	news(t, w, "initech", secretOf(t, w, "initech"), strings.Replace(approvalFor("dee"), `"id":3`, `"id":3,"own_words":true`, 1))

	out.One(t, "dee@example.com", "approval", "kai asks, in its own words, to “merge PR #17 into main”")
}

func TestAnUnconfirmedAddressGetsNoApprovals(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	dee := deeJoins(t, w, console)
	dee.Get("/account")
	dee.Post("/account/alerts/email", url.Values{"email": {"dee@example.com"}})
	dee.Get("/account")

	if r := dee.Post("/account/alerts", url.Values{"approvals": {"on"}}); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "Confirm") {
		t.Errorf("turning approvals on before confirming: %d %s", r.Status, r.Text())
	}
	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("dee"))
	if !out.None(t, "dee@example.com", "approval") {
		t.Error("an unconfirmed address got an approval email")
	}
}

// The owner's address is the Google account they created the network with:
// nothing to confirm, but approval emails are still off until they ask.
func TestTheOwnerTurnsOnApprovalEmailsForTheirGoogleAddress(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")

	page := VisibleText(console.Get("/account"))
	if !strings.Contains(page, "ada@example.com") {
		t.Fatalf("the owner's account page should show their Google address:\n%s", page)
	}
	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("ada"))
	if !out.None(t, "ada@example.com", "approval") {
		t.Fatal("approval emails should be off until the owner turns them on")
	}

	console.Post("/account/alerts", url.Values{"approvals": {"on"}})
	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("ada"))

	out.One(t, "ada@example.com", "approval", "merge PR #17 into main")
}

// deeWantsApprovals has dee join initech, confirm an address and turn
// approval emails on.
func deeWantsApprovals(t *testing.T, w *World, out *Outbox, console *Browser) *Browser {
	t.Helper()
	dee := deeJoins(t, w, console)
	dee.Get("/account")
	dee.Post("/account/alerts/email", url.Values{"email": {"dee@example.com"}})
	w.Browser("initech").Get(confirmLink.FindString(out.One(t, "dee@example.com", "Confirm").Text))
	dee.Get("/account")
	dee.Post("/account/alerts", url.Values{"approvals": {"on"}})
	return dee
}

// Someone taken out of the org (here over IRC, as the runner says) leaves
// nothing behind for whoever's given the nick later: not where their alerts
// went, nor their sign-ins.
func TestSomeoneWhoLeavesTheOrgOverIRCLeavesNothingBehind(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	dee := deeWantsApprovals(t, w, out, console)

	if status := news(t, w, "initech", secretOf(t, w, "initech"), `{"type":"member_removed","member":{"account":"dee"}}`); status != http.StatusAccepted {
		t.Fatalf("the event: %d", status)
	}

	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("dee"))
	if !out.None(t, "dee@example.com", "approval") {
		t.Error("an approval for whoever's dee now went to the old dee's address")
	}
	if r := dee.Get("/account"); r.Status == http.StatusOK {
		t.Error("the old dee is still signed in to the console")
	}
}

// The same when they're taken out from the console.
func TestSomeoneTakenOutFromTheConsoleLeavesNoAlertAddress(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	deeWantsApprovals(t, w, out, console)

	console.Get("/people/dee")
	if r := console.Post("/people/dee/remove", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("removing dee: %d %s", r.Status, r.Text())
	}

	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("dee"))
	if !out.None(t, "dee@example.com", "approval") {
		t.Error("an approval for whoever's dee now went to the old dee's address")
	}
}

// The owner never leaves: news that they did changes nothing.
func TestTheOwnerIsntForgotten(t *testing.T) {
	w, out := MailWorld(t)
	_, console := w.Created("ada@example.com", "initech")
	console.Get("/account")
	console.Post("/account/alerts", url.Values{"approvals": {"on"}})

	news(t, w, "initech", secretOf(t, w, "initech"), `{"type":"member_removed","member":{"account":"ada"}}`)

	news(t, w, "initech", secretOf(t, w, "initech"), approvalFor("ada"))
	out.One(t, "ada@example.com", "approval", "merge PR #17 into main")
	if r := console.Get("/account"); r.Status != http.StatusOK {
		t.Errorf("the owner was signed out: %d", r.Status)
	}
}

func TestTheOwnerHearsWhenTheSpendingCapIsReached(t *testing.T) {
	w, out := MailWorld(t)
	w.Created("ada@example.com", "initech")

	status := news(t, w, "initech", secretOf(t, w, "initech"), `{"type":"spend_cap","spend":{"spent":2.51,"cap":2.5}}`)

	if status != http.StatusAccepted {
		t.Fatalf("the event: %d", status)
	}
	out.One(t, "ada@example.com", "spending cap", "$2.50", "http://initech."+BaseDomain+"/spend")
}

// Only a network's own runner can say what happened on it.
func TestNewsNeedsTheNetworksSecret(t *testing.T) {
	w, out := MailWorld(t)
	w.Created("ada@example.com", "initech")

	for _, secret := range []string{"", "not-the-secret"} {
		if status := news(t, w, "initech", secret, `{"type":"spend_cap","spend":{"spent":2.51,"cap":2.5}}`); status != http.StatusUnauthorized {
			t.Errorf("an event with secret %q: %d, want 401", secret, status)
		}
	}
	if !out.None(t, "ada@example.com", "spending cap") {
		t.Error("an event without the network's secret sent an email")
	}
	if status := news(t, w, "initech", secretOf(t, w, "initech"), `{"type":"something else"}`); status != http.StatusBadRequest {
		t.Errorf("an unknown event: %d, want 400", status)
	}
}
