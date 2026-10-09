// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The base domain (guildnet.chat) is where people sign in and find their
// networks. GuildNet's hosted edition adds its own pages there through an
// extension: the landing page, the legal pages, the docs, the waitlist,
// Stripe's webhook and the admin section; and it says which pages search
// engines may index. Without one, the home page is sign-in and your
// networks, and none of it is for search engines.

// frontDoor is an extension with pages on the home page.
func frontDoor(o *Setup) {
	o.Extension = platform.ExtensionFunc(func(c *platform.Core) platform.Hooks {
		return platform.Hooks{
			Home: func(routes platform.Routes) {
				routes.Handle("GET /{$}", platform.Anyone, func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, "the front door")
				})
				routes.Handle("GET /mine", platform.SignedIn, func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, "yours")
				})
				routes.Handle("GET /admin/house", platform.Admins, func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, `<form method="post"><input type="hidden" name="csrf" value="`+c.CSRF(r)+`"></form>the house's books`)
				})
				routes.Handle("POST /admin/house", platform.Admins, func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "/admin/house", http.StatusSeeOther)
				})
			},
			IsAdmin:   func(p platform.Person) bool { return strings.EqualFold(p.Email, AdminEmail) },
			Indexable: func(path string) bool { return path == "/" },
		}
	})
}

func TestAnExtensionAddsPagesToTheHomePage(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, frontDoor)

	if r := w.Home().Get("/"); r.Status != http.StatusOK || r.Body != "the front door" {
		t.Errorf("the front door: %d %q", r.Status, r.Body)
	}
	if r := w.Home().Get("/mine"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("signed out: %d %q, want off to sign in", r.Status, r.Header.Get("Location"))
	}
	if r := w.OnHome("ada@example.com").Get("/mine"); r.Status != http.StatusOK || r.Body != "yours" {
		t.Errorf("signed in: %d %q", r.Status, r.Body)
	}
}

// The admin section isn't there at all for anyone but the admins.
func TestAnExtensionsAdminPagesAreOnlyForItsAdmins(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, frontDoor)

	if r := w.Home().Get("/admin/house"); r.Status != http.StatusNotFound {
		t.Errorf("a visitor: %d, want 404", r.Status)
	}
	if r := w.OnHome("ada@example.com").Get("/admin/house"); r.Status != http.StatusNotFound {
		t.Errorf("someone signed in: %d, want 404", r.Status)
	}
	admin := w.Admin()
	if r := admin.Get("/admin/house"); r.Status != http.StatusOK || !strings.Contains(r.Body, "the house's books") {
		t.Fatalf("the admin: %d %q", r.Status, r.Body)
	}
	if r := admin.Post("/admin/house", url.Values{"csrf": {"forged"}}); r.Status != http.StatusForbidden {
		t.Errorf("a forged form: %d, want 403", r.Status)
	}
	admin.Get("/admin/house")
	r := admin.Post("/admin/house", nil)
	if r.Status != http.StatusSeeOther {
		t.Fatalf("the admin's form: %d", r.Status)
	}
	// The next page the admin sees says it was done.
	if !strings.Contains(strings.Join(r.Header.Values("Set-Cookie"), "\n"), "=done") {
		t.Errorf("the admin isn't told it was done: %q", r.Header.Values("Set-Cookie"))
	}
}

func TestAnExtensionSaysWhatSearchEnginesMayIndex(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, frontDoor)

	if tag := w.Home().Get("/").Header.Get("X-Robots-Tag"); tag != "" {
		t.Errorf("the front door: X-Robots-Tag %q, want it indexable", tag)
	}
	if tag := w.Home().Get("/signin").Header.Get("X-Robots-Tag"); !strings.Contains(tag, "noindex") {
		t.Errorf("sign-in: X-Robots-Tag %q, want noindex", tag)
	}
}

// Without an extension: sign in and your networks, nothing for search
// engines, and none of GuildNet's own pages.
func TestTheOpenHomePageIsSignInAndYourNetworks(t *testing.T) {
	w := NewWorld(t)

	if r := w.Home().Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("a visitor at /: %d %q, want off to sign in", r.Status, r.Header.Get("Location"))
	}
	if r := w.OnHome("ada@example.com").Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks" {
		t.Errorf("signed in at /: %d %q, want their networks", r.Status, r.Header.Get("Location"))
	}
	robots := w.Home().Get("/robots.txt")
	if robots.Status != http.StatusOK || !strings.Contains(robots.Body, "Disallow: /") {
		t.Errorf("robots.txt: %d %q, want everything disallowed", robots.Status, robots.Body)
	}
	for _, path := range []string{"/signin", "/networks", "/robots.txt"} {
		if tag := w.Home().Get(path).Header.Get("X-Robots-Tag"); !strings.Contains(tag, "noindex") {
			t.Errorf("%s: X-Robots-Tag %q, want noindex", path, tag)
		}
	}
	for _, path := range []string{"/privacy", "/terms", "/refunds", "/docs/agents", "/docs/under-the-hood", "/sitemap.xml", "/admin", "/admin/waitlist"} {
		if r := w.Admin().Get(path); r.Status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, r.Status)
		}
	}
	if r := w.Home().Do("POST", "/waitlist", url.Values{"email": {"eve@example.com"}}); r.Status != http.StatusNotFound {
		t.Errorf("the waitlist: %d, want 404", r.Status)
	}
	if r := w.Home().Do("POST", "/stripe/webhook", nil); r.Status != http.StatusNotFound {
		t.Errorf("Stripe's webhook: %d, want 404", r.Status)
	}
	if strings.Contains(w.OnHome("ada@example.com").Get("/networks").Body, `href="/admin"`) {
		t.Error("the home page links to an admin section that isn't there")
	}
}
