// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A self-hosted GuildNet can go by its own name, with its own addresses to
// write to: the console, the home page and the email it sends say so. It's
// GuildNet, at GuildNet's addresses on its own domain, unless it's told
// otherwise.

func acmeBrand(o *Setup) {
	o.Brand = platform.Brand{Name: "Acme Chat", Hello: "hi@acme.test", Support: "help@acme.test"}
}

// mailTo waits a moment for an email to addr about subject.
func mailTo(t *testing.T, out *Outbox, addr, about string) mail.Message {
	t.Helper()
	for range 40 {
		for _, m := range out.To(addr) {
			if strings.Contains(m.Subject, about) {
				return m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no email to %s about %q", addr, about)
	return mail.Message{}
}

func TestABrandNamesTheConsoleTheHomePageAndTheEmail(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), acmeBrand)
	_, console := w.Created("ada@example.com", "initech")

	// The console names nothing but the network; it mustn't say GuildNet.
	if page := console.Get("/"); strings.Contains(page.Body, "GuildNet") {
		t.Errorf("the console says GuildNet:\n%s", page.Text())
	}
	for name, page := range map[string]Response{
		"its sign-in page": w.Browser("initech").Get("/signin"),
		"the home sign-in": w.Home().Get("/signin"),
		"your networks":    SignedInHome(w, "bob@example.com").Get("/networks"),
		"a page not found": w.Home().Get("/nothing-here"),
	} {
		if !strings.Contains(page.Body, "Acme Chat") || strings.Contains(page.Body, "GuildNet") {
			t.Errorf("%s doesn't go by Acme Chat alone:\n%s", name, page.Text())
		}
	}
	if page := w.Home().Get("/signin").Body; !strings.Contains(page, "hi@acme.test") {
		t.Errorf("the home page doesn't give the brand's address to write to:\n%s", page)
	}

	welcome := mailTo(t, out, "ada@example.com", "is ready")
	for part, s := range map[string]string{"subject": welcome.Subject, "text": welcome.Text, "HTML": welcome.HTML} {
		if !strings.Contains(s, "Acme Chat") || strings.Contains(s, "GuildNet") {
			t.Errorf("the welcome email's %s doesn't go by Acme Chat alone:\n%s", part, s)
		}
	}
}

func TestItsGuildNetUnlessToldOtherwise(t *testing.T) {
	w := NewWorld(t)

	page := w.Home().Get("/signin").Body
	if !strings.Contains(page, "<title>Sign in · GuildNet</title>") || !strings.Contains(page, "hello@"+BaseDomain) {
		t.Errorf("the home sign-in isn't GuildNet's, at its own domain:\n%s", page)
	}
}

// A brand can have its own logo, colours (a stylesheet laid over the
// console's), icons and email footer; without them it's GuildNet's.
func TestABrandsOwnLogoColoursIconsAndFooter(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), func(o *Setup) {
		o.Brand = platform.Brand{Name: "Acme Chat", Tagline: "chat for Acme's crews", Files: fstest.MapFS{
			"logo.svg":             {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><title>acme</title></svg>`)},
			"brand.css":            {Data: []byte(":root { --accent: #c03; }")},
			"favicon.ico":          {Data: []byte("acme-icon")},
			"apple-touch-icon.png": {Data: []byte("acme-touch-icon")},
		}}
	})

	for name, page := range map[string]Response{"the home sign-in": w.Home().Get("/signin"), "a console's sign-in": w.Browser("acme").Get("/signin")} {
		if !strings.Contains(page.Body, `<link rel="stylesheet" href="/static/brand.css?v=`) {
			t.Errorf("%s doesn't use the brand's colours:\n%s", name, page.Body)
		}
	}
	home := w.Home().Get("/signin").Body
	if !strings.Contains(home, `<img class="logo" src="/static/logo.svg?v=`) || strings.Contains(home, `class="shield"`) {
		t.Errorf("the home page doesn't show the brand's logo:\n%s", home)
	}
	for path, want := range map[string]string{"/favicon.ico": "acme-icon", "/apple-touch-icon.png": "acme-touch-icon", "/static/brand.css": "--accent"} {
		if r := w.Home().Get(path); r.Status != 200 || !strings.Contains(r.Body, want) {
			t.Errorf("%s: %d %q", path, r.Status, r.Body)
		}
	}

	w.Created("ada@example.com", "initech")
	welcome := mailTo(t, out, "ada@example.com", "is ready")
	if !strings.Contains(welcome.HTML, "chat for Acme&#39;s crews") || strings.Contains(welcome.HTML, "an AI team you run like a real one") {
		t.Errorf("the email's footer isn't the brand's:\n%s", welcome.HTML)
	}
}

func TestWithoutItsOwnFilesABrandLooksLikeGuildNet(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, acmeBrand)

	page := w.Home().Get("/signin").Body

	if !strings.Contains(page, `class="shield"`) || strings.Contains(page, "brand.css") {
		t.Errorf("the home page should have GuildNet's crest and colours:\n%s", page)
	}
}
