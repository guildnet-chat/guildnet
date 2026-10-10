// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// How to connect an IRC app, shown where people look for it: the network's
// page once it's ready, and a page in its console.

func TestTheConsoleSaysHowToConnectAnIRCApp(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.IRCPort = 6697 })
	ada := w.Browser("acme").SignedIn("ada")
	if !strings.Contains(ada.Get("/").Body, `href="/connect"`) {
		t.Error("the console doesn't link to connecting an IRC app")
	}
	page := ada.Get("/connect")
	if page.Status != http.StatusOK {
		t.Fatalf("connect page: %d", page.Status)
	}
	for _, want := range []string{"acme." + BaseDomain, "6697", "TLS", "+6697", "SASL", "ada@phone"} {
		if !strings.Contains(page.Text(), want) {
			t.Errorf("the connect page doesn't say %q:\n%s", want, page.Text())
		}
	}
}

func TestWithoutIRCForAppsTheConsoleSaysSo(t *testing.T) {
	w := NewWorld(t) // no IRC port for apps, as on a developer's machine
	page := w.Browser("acme").SignedIn("ada").Get("/connect")
	if page.Status != http.StatusOK || strings.Contains(page.Text(), "6697") || !strings.Contains(page.Text(), "can't connect") {
		t.Errorf("connect page without IRC for apps: %d\n%s", page.Status, page.Text())
	}
}

func TestANewNetworksPageSaysHowToConnectAnIRCApp(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.IRCPort = 6697 })
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	b.Post("/networks", CreateForm("initech"))
	ready := b.WaitReady("initech")
	for _, want := range []string{"initech." + BaseDomain, "+6697", "SASL"} {
		if !strings.Contains(ready.Text(), want) {
			t.Errorf("the ready page doesn't say %q:\n%s", want, ready.Text())
		}
	}
}
