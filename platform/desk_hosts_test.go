// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The Connectors page shows the hosts the network's desks may reach past
// the usual ones: the owner adds and removes them; anyone else asks the
// owner for one. The list is the runner's, which logs each change and
// tells the platform, which emails the owner.

func deskWorld(t *testing.T) *World {
	t.Helper()
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey}
	})
}

func TestTheOwnerChoosesHostsOnTheConnectorsPage(t *testing.T) {
	w := deskWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	page := VisibleText(ada.Get("/connectors"))
	for _, want := range []string{"Hosts desks may reach", "GitHub", "npm", "No other hosts yet"} {
		if !strings.Contains(page, want) {
			t.Errorf("the section should say %q:\n%s", want, page)
		}
	}
	if r := ada.Post("/desk-hosts", url.Values{"host": {"api.stripe.com"}}); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/connectors#hosts" {
		t.Fatalf("adding api.stripe.com: %d %s", r.Status, r.Text())
	}
	if page := VisibleText(ada.Get("/connectors")); !strings.Contains(page, "api.stripe.com") {
		t.Errorf("the page should list api.stripe.com:\n%s", page)
	}
	ada.Get("/connectors")
	if r := ada.Post("/desk-hosts", url.Values{"host": {"pastebin.com"}}); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "blocked") {
		t.Errorf("adding pastebin.com: %d, want it refused as blocked:\n%s", r.Status, VisibleText(r))
	}

	ada.Get("/connectors")
	if r := ada.Post("/desk-hosts/api.stripe.com/remove", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("removing it: %d %s", r.Status, r.Text())
	}
	if page := VisibleText(ada.Get("/connectors")); strings.Contains(page, "api.stripe.com") {
		t.Errorf("api.stripe.com is still listed:\n%s", page)
	}
}

func TestSomeoneElseAsksTheOwnerForAHostOnTheConnectorsPage(t *testing.T) {
	w := deskWorld(t)
	bo := w.Browser("acme").SignedIn("bo")

	page := bo.Get("/connectors")
	if strings.Contains(page.Body, `action="/desk-hosts"`) {
		t.Error("bo, who isn't the owner, is offered the form to add a host")
	}
	if !strings.Contains(page.Body, `action="/desk-hosts/ask"`) {
		t.Errorf("bo should be offered a way to ask:\n%s", VisibleText(page))
	}
	if r := bo.Post("/desk-hosts", url.Values{"host": {"api.stripe.com"}}); r.Status == http.StatusSeeOther {
		t.Error("bo added a host")
	}

	bo.Get("/connectors")
	r := bo.Post("/desk-hosts/ask", url.Values{"host": {"api.stripe.com"}, "why": {"the payments integration"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("asking: %d %s", r.Status, VisibleText(r))
	}
	if page := VisibleText(bo.Get(r.Header.Get("Location"))); !strings.Contains(page, "Asked ada") {
		t.Errorf("bo should be told the owner was asked:\n%s", page)
	}
	if page := VisibleText(acmeOwner(t, w).Get("/approvals")); !strings.Contains(page, "let desks reach api.stripe.com") {
		t.Errorf("ada's approvals should have bo's ask:\n%s", page)
	}
}

func acmeOwner(t *testing.T, w *World) *Browser {
	t.Helper()
	return w.Browser("acme").SignedIn("ada")
}

// Without desks of the console's own, there's no proxy, and no list.
func TestWithoutDesksThereAreNoHostsToChoose(t *testing.T) {
	w := NewWorld(t)
	if page := VisibleText(w.Browser("acme").SignedIn("ada").Get("/connectors")); strings.Contains(page, "Hosts desks may reach") {
		t.Errorf("the page offers hosts without desks:\n%s", page)
	}
}

func TestTheOwnerIsEmailedEachChangeToTheHosts(t *testing.T) {
	w, out := MailWorld(t)
	w.Created("ada@example.com", "initech")

	status := news(t, w, "initech", secretOf(t, w, "initech"),
		`{"type":"desk_host","desk_host":{"host":"api.stripe.com","change":"added","by":"ada","asked":"kai","why":"to read the webhook docs"}}`)

	if status != http.StatusAccepted {
		t.Fatalf("the news: %d", status)
	}
	out.One(t, "ada@example.com", "api.stripe.com", "kai", "to read the webhook docs", "http://initech."+BaseDomain+"/connectors#hosts")
}
