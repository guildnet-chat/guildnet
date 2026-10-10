// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network set up without the home page (as a self-hosted one is) has an
// owner all the same: its owner's IRC account. They manage its model keys
// in Settings, and its security alerts go to the address they confirmed on
// their account page.

// ownedByNick makes network's owner owner, with nobody behind it on the
// home page, as first-run setup leaves it.
func ownedByNick(t *testing.T, w *World, network, owner string) {
	t.Helper()
	n, err := w.Store.Network(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	n.OwnerNick, n.Template, n.Timezone = owner, "team", "UTC"
	n.Keys.Set(models.Anthropic, "sk-ant-api03-"+network+"-0123456789")
	if err := w.Store.UpdateNetwork(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

func TestAnOwnerWithoutAHomePageAccountManagesTheModelKeys(t *testing.T) {
	w := NewWorld(t)
	ownedByNick(t, w, "acme", "ada")
	ada := w.Browser("acme").SignedIn("ada")

	if r := ada.Get("/settings"); r.Status != http.StatusOK || !strings.Contains(r.Text(), "Anthropic") {
		t.Fatalf("settings: %d %s", r.Status, r.Text())
	}
	r := ada.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-acme-0123456789"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("adding a key: %d %s", r.Status, r.Text())
	}
	if s, _ := w.Host.Spec("acme"); s.ModelKeys["openai"] != "sk-proj-acme-0123456789" {
		t.Errorf("the network runs with %v", s.ModelKeys)
	}
	if r := w.Browser("acme").SignedIn("bo").Get("/settings"); r.Status != http.StatusForbidden {
		t.Errorf("bo sees settings: %d", r.Status)
	}
}

func TestANetworkWithNoOwnerHasNoSettings(t *testing.T) {
	w := NewWorld(t) // acme came from the environment: nobody owns it here

	if r := w.Browser("acme").SignedIn("ada").Get("/settings"); r.Status != http.StatusNotFound {
		t.Errorf("settings: %d", r.Status)
	}
}

func TestAnOwnerWithoutAHomePageAccountGetsSecurityAlertsAtTheirConfirmedAddress(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out))
	ownedByNick(t, w, "acme", "ada")
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/settings")
	ada.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-acme-0123456789"}})
	if len(out.To("ada@acme.example")) != 0 {
		t.Fatal("an alert went to an address before it was given")
	}

	confirmAlerts(t, out, ada, "ada@acme.example")
	ada.Get("/settings")
	ada.Post("/settings/keys", url.Values{"provider": {"openai"}, "model_key": {"sk-proj-acme-9876543210"}})

	m := out.One(t, "ada@acme.example", "OpenAI key was replaced", "ada", "If that wasn't you", "acme's console")
	if strings.Contains(m.Text, "network's page") {
		t.Errorf("the alert sends ada to a home page they don't have:\n%s", m.Text)
	}
}

// confirmAlerts has whoever b is signed in as confirm address for their
// alerts.
func confirmAlerts(t *testing.T, out *Outbox, b *Browser, address string) {
	t.Helper()
	b.Get("/account")
	if r := b.Post("/account/alerts/email", url.Values{"email": {address}}); r.Status != http.StatusSeeOther {
		t.Fatalf("giving an address: %d %s", r.Status, r.Text())
	}
	b.Get(confirmLink.FindString(out.One(t, address, "Confirm your email").Text))
}

func TestAServiceThatNeedsReconnectingSendsTheOwnerToTheConsole(t *testing.T) {
	out := &Outbox{}
	w, f := newConnectorWorld(t, WithMail(out))
	ownedByNick(t, w, "acme", "ada")
	ada := w.Browser("acme").SignedIn("ada")
	confirmAlerts(t, out, ada, "ada@acme.example")
	authorize := connectFromConsole(t, ada, "linear")
	u, _ := url.Parse(authorize)
	ada.Get("/oauth/callback?" + url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {f.srv.URL}}.Encode())
	f.mu.Lock()
	f.refuse = true
	f.mu.Unlock()
	w.Clock.Advance(time.Hour)

	BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/connectors/linear/token")

	out.One(t, "ada@acme.example", "Linear needs reconnecting", "acme's Connectors page", "http://acme."+BaseDomain+"/connectors")
}
