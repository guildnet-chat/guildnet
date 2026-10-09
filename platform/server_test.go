// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A self-hosted install's Server page, in its network's console: what's
// running, whether each part answers, what's set up and what isn't, and
// what to back up. The owner uses it; whoever may change things across the
// whole org sees it; nobody else does. Networks GuildNet hosts have none.

// joins has nick join the self-hosted network with an invite from the
// owner, signed in to its console.
func joins(t *testing.T, w *World, owner *Browser, nick string) *Browser {
	t.Helper()
	owner.Get("/people")
	link := JoinLink.FindString(owner.Post("/people/invite", url.Values{"role": {"Engineer"}}).Body)
	b := w.Browser(SelfHosted)
	b.Get(link)
	if r := b.Post(link, url.Values{"nick": {nick}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("%s joining: %d %s", nick, r.Status, r.Text())
	}
	return b
}

func TestTheOwnerSeesTheServerPage(t *testing.T) {
	w := NewSelfHostedWorld(t)
	_, ada := w.SetUp()

	page := ada.Get("/server")

	if page.Status != http.StatusOK {
		t.Fatalf("the server page: %d %s", page.Status, page.Text())
	}
	for _, says := range []string{"Version", "IRC server", "Runner", "Running", "Email", "Off", "GitHub", "Locked out", "reset-owner", "Back up", ".env"} {
		if !strings.Contains(page.Text(), says) {
			t.Errorf("the page should say %q:\n%s", says, page.Text())
		}
	}
	// The IRC server says who's on it, and in how many channels.
	if !regexp.MustCompile(`IRC server</td><td>[^\n]*Running · \d+ connected · \d+ channels`).MatchString(page.Body) {
		t.Errorf("the IRC server's line should say how many are connected, in how many channels:\n%s", page.Text())
	}
	for _, link := range []string{`href="/server"`, `href="/settings"`} {
		if !strings.Contains(page.Body, link) {
			t.Errorf("the console's menu should have %s", link)
		}
	}
}

func TestOrgWideAdminsSeeTheServerPageAndNobodyElseDoes(t *testing.T) {
	w := NewSelfHostedWorld(t)
	_, ada := w.SetUp()
	dee := joins(t, w, ada, "dee")

	if r := dee.Get("/server"); r.Status != http.StatusNotFound || strings.Contains(r.Body, `href="/server"`) {
		t.Errorf("dee, a member: %d", r.Status)
	}
	ada.Get("/people/dee")
	if r := ada.Post("/people/dee/access", url.Values{"level": {"Admin"}, "give": {"1"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("making dee an admin: %d %s", r.Status, r.Text())
	}
	page := dee.Get("/server")
	if page.Status != http.StatusOK || !strings.Contains(page.Text(), "Version") {
		t.Fatalf("dee, an admin: %d %s", page.Status, page.Text())
	}
	if strings.Contains(page.Body, `action="/server/test-email"`) {
		t.Error("an admin who isn't the owner can send test emails")
	}
}

func TestTheOwnerSendsATestEmail(t *testing.T) {
	out := &Outbox{}
	w := NewSelfHostedWorld(t, WithMail(out))
	_, ada := w.SetUp()
	ada.Get("/server")
	if r := ada.Post("/server/test-email", nil); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "account page") {
		t.Errorf("with no address confirmed: %d %s", r.Status, r.Text())
	}
	confirmAlerts(t, out, ada, "ada@hq.example")
	ada.Get("/server")

	if r := ada.Post("/server/test-email", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("sending a test email: %d %s", r.Status, r.Text())
	}

	out.One(t, "ada@hq.example", "test email", "hq")
}

func TestAHostedNetworkHasNoServerPage(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")

	if r := console.Get("/server"); r.Status != http.StatusNotFound || strings.Contains(r.Body, `href="/server"`) {
		t.Errorf("a hosted network's server page: %d", r.Status)
	}
}
