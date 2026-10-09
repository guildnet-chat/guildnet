// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The design system: every page works in light and dark, and its typefaces
// come from the site itself, never a third party that could track visitors.

func TestEveryPageOffersLightAndDark(t *testing.T) {
	w := NewWorld(t)
	pages := map[string]Response{
		"the home page": w.Home().Get("/signin"),
		"signing in":    w.Browser("acme").Get("/signin"),
		"the console":   w.Browser("acme").SignedIn("ada").Get("/"),
	}

	for name, r := range pages {
		if !regexp.MustCompile(`<script src="/static/theme.js\?v=[0-9a-f]+"></script>`).MatchString(r.Body) {
			t.Errorf("%s should load the theme script", name)
		}
		if !strings.Contains(r.Body, `data-theme-toggle`) {
			t.Errorf("%s should have a light and dark switch", name)
		}
		if !regexp.MustCompile(`<link rel="stylesheet" href="/static/tokens.css\?v=[0-9a-f]+">`).MatchString(r.Body) {
			t.Errorf("%s should use the design tokens", name)
		}
	}
	js := w.Home().Get("/static/theme.js")
	if js.Status != http.StatusOK || !strings.Contains(js.Header.Get("Content-Type"), "javascript") {
		t.Errorf("theme.js: %d %s", js.Status, js.Header.Get("Content-Type"))
	}
	if strings.Contains(js.Body, "cookie") {
		t.Error("the theme is remembered in the browser only, never in a cookie")
	}
}

// GuildNet has a mark: every page names it as the tab's icon, and pages that
// show the name show the mark beside it. Browsers and phones that ask for an
// icon at the usual addresses, on any of GuildNet's sites, get it there.
func TestGuildNetHasAnIcon(t *testing.T) {
	w := NewWorld(t)
	pages := map[string]Response{
		"the home page": w.Home().Get("/signin"),
		"signing in":    w.Browser("acme").Get("/signin"),
		"the console":   w.Browser("acme").SignedIn("ada").Get("/"),
	}
	for name, r := range pages {
		for _, want := range []*regexp.Regexp{
			// The .ico only: its 16px icon is drawn on whole pixels, sharper
			// than a browser scaling the SVG down. Linked at an address that
			// changes with it, as browsers keep icons (and failures to find
			// one) for a long time.
			regexp.MustCompile(`<link rel="icon" href="/static/favicon.ico\?v=[0-9a-f]+" sizes="16x16 32x32 48x48">`),
			regexp.MustCompile(`<link rel="apple-touch-icon" href="/static/apple-touch-icon.png\?v=[0-9a-f]+">`),
		} {
			if !want.MatchString(r.Body) {
				t.Errorf("%s should have %s", name, want)
			}
		}
	}
	for _, name := range []string{"the home page", "signing in"} {
		if !strings.Contains(pages[name].Body, `class="logo"`) {
			t.Errorf("%s should show the mark beside the name", name)
		}
	}

	for _, b := range []*Browser{w.Home(), w.Browser("acme")} {
		for path, want := range map[string]struct{ kind, magic string }{
			"/favicon.ico":                 {"image/x-icon", "\x00\x00\x01\x00"},
			"/apple-touch-icon.png":        {"image/png", "\x89PNG"},
			"/static/favicon.ico":          {"image/x-icon", "\x00\x00\x01\x00"},
			"/static/apple-touch-icon.png": {"image/png", "\x89PNG"},
		} {
			r := b.Get(path)
			if r.Status != http.StatusOK || r.Header.Get("Content-Type") != want.kind || !strings.HasPrefix(r.Body, want.magic) {
				t.Errorf("%s%s: %d %q", b.Host, path, r.Status, r.Header.Get("Content-Type"))
			}
		}
	}
	if svg := w.Home().Get("/static/logo.svg"); svg.Status != http.StatusOK || svg.Header.Get("Content-Type") != "image/svg+xml" {
		t.Errorf("logo.svg: %d %q", svg.Status, svg.Header.Get("Content-Type"))
	}
}

// Static files are cached for a year, so each release must point pages at
// new addresses, or browsers would keep last release's styles.
func TestStaticFilesAreAddressedByWhatsInThem(t *testing.T) {
	w := NewWorld(t)
	v := w.Home()

	page := v.Get("/signin")

	m := regexp.MustCompile(`href="(/static/tokens.css\?v=([0-9a-f]+))"`).FindStringSubmatch(page.Body)
	if m == nil {
		t.Fatal("the page should link its stylesheet with a version")
	}
	css := v.Get(m[1])
	if css.Status != http.StatusOK || !strings.Contains(css.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("%s: %d %q", m[1], css.Status, css.Header.Get("Cache-Control"))
	}
	sum := sha256.Sum256([]byte(css.Body))
	if !strings.HasPrefix(hex.EncodeToString(sum[:]), m[2]) {
		t.Errorf("version %s isn't taken from the file's contents, so it wouldn't change when they do", m[2])
	}
}

func TestTypefacesComeFromTheSiteItself(t *testing.T) {
	w := NewWorld(t)
	v := w.Home()

	css := v.Get("/static/tokens.css")

	if css.Status != http.StatusOK || !strings.Contains(css.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("tokens.css: %d %s", css.Status, css.Header.Get("Content-Type"))
	}
	fonts := regexp.MustCompile(`url\("(/static/fonts/[^"]+\.woff2)"\)`).FindAllStringSubmatch(css.Body, -1)
	if len(fonts) < 5 {
		t.Fatalf("want Space Grotesk and JetBrains Mono in several weights, got %d fonts", len(fonts))
	}
	for _, f := range fonts {
		r := v.Get(f[1])
		if r.Status != http.StatusOK || r.Header.Get("Content-Type") != "font/woff2" {
			t.Errorf("%s: %d %q", f[1], r.Status, r.Header.Get("Content-Type"))
		}
	}
	for _, licence := range []string{"/static/fonts/SpaceGrotesk-OFL.txt", "/static/fonts/JetBrainsMono-OFL.txt"} {
		if r := v.Get(licence); r.Status != http.StatusOK || !strings.Contains(r.Body, "SIL Open Font License") {
			t.Errorf("%s should ship with the fonts: %d", licence, r.Status)
		}
	}
	page := v.Get("/")
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "font-src 'self'") {
		t.Errorf("CSP %q should allow fonts from the site only", csp)
	}
	if strings.Contains(page.Body, "fonts.googleapis") || strings.Contains(page.Body, "fonts.gstatic") {
		t.Error("the page shouldn't load fonts from Google")
	}
}

func TestTheOrgChartShowsWhatsWaitingOnYou(t *testing.T) {
	w := NewWorld(t)
	id := w.Networks["acme"].RaiseApproval(t, "kai", "ada", "merge PR #17 'Retry webhooks' into main")

	ada := w.Browser("acme").SignedIn("ada")
	byAda := ada.Get("/")
	byBo := w.Browser("acme").SignedIn("bo").Get("/")

	if !strings.Contains(byAda.Body, "merge PR #17") || !strings.Contains(byAda.Body, `action="/approvals/`+Itoa(id)+`/approve"`) {
		t.Errorf("ada should see the approval, ready to decide, beside the org chart:\n%s", byAda.Body)
	}
	if !strings.Contains(byAda.Body, `Approvals <span class="count">1</span>`) {
		t.Error("the menu should count what's waiting on ada")
	}
	if strings.Contains(byBo.Body, "merge PR #17") {
		t.Error("bo isn't in kai's reporting line, so shouldn't see its approval")
	}

	r := ada.Post("/approvals/"+Itoa(id)+"/approve", url.Values{"back": {"/"}})

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Errorf("approving from the org chart: %d to %q, want back to the org chart", r.Status, r.Header.Get("Location"))
	}
	if a, _ := w.Networks["acme"].Store.Approval(context.Background(), id); a.Status != "approved" {
		t.Errorf("approval = %+v", a)
	}
}

func TestDecidingNeverSendsYouOffTheSite(t *testing.T) {
	w := NewWorld(t)
	id := w.Networks["acme"].RaiseApproval(t, "kai", "ada", "merge PR #17 into main")

	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/")

	r := ada.Post("/approvals/"+Itoa(id)+"/deny", url.Values{"back": {"//evil.example/"}})

	if r.Header.Get("Location") != "/approvals" {
		t.Errorf("went to %q; only the org chart or the approvals page", r.Header.Get("Location"))
	}
}
