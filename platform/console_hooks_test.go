// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// What an extension adds to a network's console: sections on its owner's
// Settings, pages of its own (signed in, like the console's), places its
// forms may send people (a payment page), and a say before a network is
// deleted. The open console has none of GuildNet's plans or billing.

// house is an extension a test controls.
type house struct {
	refuse        string // why a network can't be deleted yet
	deleteErr     error
	brokenSection bool
	notice        string // atop the owner's org chart
	brokenNotice  bool
	core          *platform.Core
}

func (h *house) hooks(c *platform.Core) platform.Hooks {
	h.core = c
	return platform.Hooks{
		Settings: []platform.SettingsSection{
			{Render: func(r *http.Request, n platform.Network) (template.HTML, error) {
				return template.HTML(`<section class="card" id="house"><h2>House rules</h2><p>` + template.HTMLEscapeString(n.Name) + ` keeps the house rules.</p></section>`), nil
			}},
			{Render: func(r *http.Request, n platform.Network) (template.HTML, error) {
				if h.brokenSection {
					return "", errors.New("the house ledger is down")
				}
				return "", nil // nothing to show
			}},
		},
		Console: func(routes platform.Routes) {
			routes.Handle("POST /settings/house", platform.SignedIn, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "house rules kept for "+c.NetworkOf(r).Name)
			})
		},
		Notice: func(r *http.Request, n platform.Network) (template.HTML, error) {
			if h.brokenNotice {
				return "", errors.New("the house ledger is down")
			}
			return template.HTML(h.notice), nil
		},
		FormAction: []string{"https://pay.example.com"},
		BeforeDelete: func(ctx context.Context, n platform.Network) (string, error) {
			return h.refuse, h.deleteErr
		},
	}
}

func withHouse(h *house) func(*Setup) {
	return func(o *Setup) {
		o.Extension = platform.ExtensionFunc(h.hooks)
	}
}

func houseWorld(t *testing.T, h *house) *World {
	t.Helper()
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, withHouse(h))
}

func TestAnExtensionAddsSectionsToSettings(t *testing.T) {
	h := &house{}
	w := houseWorld(t, h)
	_, console := w.Created("ada@example.com", "initech")

	page := console.Get("/settings")

	if page.Status != http.StatusOK || !strings.Contains(page.Body, `<section class="card" id="house"><h2>House rules</h2><p>initech keeps the house rules.</p></section>`) {
		t.Errorf("settings: %d, want the house's section as it wrote it:\n%s", page.Status, page.Body)
	}
	if strings.Contains(page.Text(), "Plan and desk hours") {
		t.Error("settings show GuildNet's plans without them")
	}

	// A section that fails is left out; Settings still work.
	h.brokenSection = true
	if page := console.Get("/settings"); page.Status != http.StatusOK || !strings.Contains(page.Text(), "House rules") || !strings.Contains(page.Text(), "Company timezone") {
		t.Errorf("with a failing section: %d\n%s", page.Status, page.Text())
	}
}

// A notice is something for the owner to act on (GuildNet's: desks out of
// hours), atop the org chart, where they arrive.
func TestAnExtensionPutsANoticeAtopTheOwnersOrgChart(t *testing.T) {
	h := &house{notice: `<p class="banner" id="house-notice">The house is closed.</p>`}
	w := houseWorld(t, h)
	_, console := w.Created("ada@example.com", "initech")

	if page := console.Get("/"); page.Status != http.StatusOK || !strings.Contains(page.Body, h.notice) {
		t.Errorf("the owner's org chart: %d, want the house's notice as it wrote it:\n%s", page.Status, page.Body)
	}
	if page := console.Get("/teams"); strings.Contains(page.Body, "house-notice") {
		t.Error("the notice is on every page, not only the org chart")
	}
	// Only for whoever owns the network: a network the host doesn't run has
	// nobody to tell.
	if page := w.Browser("acme").SignedIn("ada").Get("/"); strings.Contains(page.Body, "house-notice") {
		t.Error("acme, which nobody owns here, shows the notice")
	}

	// A notice that fails is left out; the org chart still shows.
	h.brokenNotice = true
	if page := console.Get("/"); page.Status != http.StatusOK || strings.Contains(page.Body, "house-notice") || !strings.Contains(page.Text(), "Org chart") {
		t.Errorf("with a failing notice: %d\n%s", page.Status, page.Text())
	}
}

// An extension can ask whether sandboxd answers (its VM is on), as the VM's
// own power-off timer does: not desk use, so it doesn't keep the VM up.
func TestAnExtensionAsksWhetherTheSandboxIsUp(t *testing.T) {
	var asked atomic.Value
	sb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.Method + " " + r.URL.Path)
		io.WriteString(w, `{"idle_seconds":42}`)
	}))
	h := &house{}
	NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, withHouse(h), func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: sb.URL, Key: SandboxKey}
	})

	if !h.core.SandboxUp(context.Background()) || asked.Load() != "GET /v1/idle" {
		t.Errorf("with sandboxd answering: down, having asked %v", asked.Load())
	}
	sb.Close()
	if h.core.SandboxUp(context.Background()) {
		t.Error("with sandboxd gone: up")
	}

	without := &house{}
	houseWorld(t, without)
	if without.core.SandboxUp(context.Background()) {
		t.Error("with no sandbox at all: up")
	}
}

func TestAnExtensionAddsPagesToTheConsole(t *testing.T) {
	w := houseWorld(t, &house{})
	_, console := w.Created("ada@example.com", "initech")

	console.Get("/settings")
	if r := console.Post("/settings/house", nil); r.Status != http.StatusOK || r.Body != "house rules kept for initech" {
		t.Errorf("signed in: %d %q", r.Status, r.Body)
	}
	if r := console.Post("/settings/house", url.Values{"csrf": {"forged"}}); r.Status != http.StatusForbidden {
		t.Errorf("with a forged form: %d, want 403", r.Status)
	}
	if r := w.Browser("initech").Post("/settings/house", nil); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("signed out: %d %q, want off to sign in", r.Status, r.Header.Get("Location"))
	}
}

func TestAnExtensionMayLetFormsGoElsewhere(t *testing.T) {
	w := houseWorld(t, &house{})
	_, console := w.Created("ada@example.com", "initech")

	if csp := console.Get("/settings").Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://pay.example.com") {
		t.Errorf("signed in: %q, want forms allowed to the payment page", csp)
	}
	if csp := w.Browser("initech").Get("/signin").Header.Get("Content-Security-Policy"); strings.Contains(csp, "pay.example.com") {
		t.Errorf("signed out: %q", csp)
	}
}

func TestAnExtensionCanHoldOffDeletingANetwork(t *testing.T) {
	h := &house{refuse: "initech still owes the house"}
	w := houseWorld(t, h)
	_, console := w.Created("ada@example.com", "initech")

	console.Get("/settings")
	if r := console.Post("/settings/delete", url.Values{"confirm": {"initech"}}); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "initech still owes the house") {
		t.Errorf("refused: %d %s", r.Status, r.Text())
	}
	h.refuse, h.deleteErr = "", errors.New("the house ledger is down")
	console.Get("/settings")
	if r := console.Post("/settings/delete", url.Values{"confirm": {"initech"}}); r.Status != http.StatusInternalServerError || !strings.Contains(r.Text(), "Couldn't delete it") {
		t.Errorf("when the house can't say: %d %s", r.Status, r.Text())
	}
	if len(w.Host.Removed) > 0 {
		t.Fatal("removed while the house held off")
	}

	h.deleteErr = nil
	console.Get("/settings")
	if r := console.Post("/settings/delete", url.Values{"confirm": {"initech"}}); r.Status != http.StatusOK {
		t.Errorf("allowed: %d %s", r.Status, r.Text())
	}
	w.Host.WaitRemoved(t, "initech")
}

// The open console: no plans, no billing, no payment pages.
func TestTheOpenConsoleHasNoPlansOrBilling(t *testing.T) {
	w := NewWorld(t)
	_, console := w.Created("ada@example.com", "initech")

	page := console.Get("/settings")
	if page.Status != http.StatusOK || strings.Contains(page.Text(), "Plan and desk hours") || strings.Contains(page.Text(), "Credits") {
		t.Errorf("settings: %d\n%s", page.Status, page.Text())
	}
	if csp := page.Header.Get("Content-Security-Policy"); strings.Contains(csp, "stripe") {
		t.Errorf("CSP lets forms go to Stripe: %q", csp)
	}
	for _, path := range []string{"/settings/billing/plan", "/settings/billing/credits", "/settings/billing/portal", "/settings/billing/currency"} {
		console.Get("/settings")
		if r := console.Post(path, nil); r.Status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, r.Status)
		}
	}
	console.Get("/settings")
	if r := console.Post("/settings/delete", url.Values{"confirm": {"initech"}}); r.Status != http.StatusOK {
		t.Errorf("deleting: %d %s", r.Status, r.Text())
	}
}
