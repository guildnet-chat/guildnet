// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// GuildNet's console on its own, as anyone can run it (setup.Open), has
// none of the hosted edition and doesn't point at it: its pages link only
// to what's there. (What it does without the hosted edition is tested
// beside each hook: no plans or billing, console_hooks_test; sign-in and
// your networks on the home page, home_hooks_test; one network, its
// checklist and jobs, closed_stores_test; desks uncounted,
// extension_test; no model gateway, models_hooks_test.)

// The hosted edition's site has pricing, docs, legal pages and an admin
// section; the open one has none of them, so its header and footer don't
// link to them.
func TestTheOpenSiteLinksOnlyToWhatsThere(t *testing.T) {
	w := NewWorld(t)

	for name, page := range map[string]Response{
		"sign-in":          w.Home().Get("/signin"),
		"your networks":    SignedInHome(w, "bob@example.com").Get("/networks"),
		"a page not found": w.Home().Get("/nothing-here"),
	} {
		for _, closed := range []string{`href="/#`, `href="/docs/`, `href="/terms"`, `href="/refunds"`, `href="/privacy"`, `href="/admin"`, `href="#beta"`} {
			if strings.Contains(page.Body, closed) {
				t.Errorf("%s links to what isn't here: %s", name, closed)
			}
		}
		for _, want := range []string{`class="site-header"`, `class="site-footer"`, `href="mailto:hello@` + BaseDomain + `"`} {
			if !strings.Contains(page.Body, want) {
				t.Errorf("%s doesn't have %s:\n%s", name, want, page.Body)
			}
		}
	}
	if page := w.Home().Get("/signin"); page.Status != http.StatusOK || !strings.Contains(page.Body, `<a class="brand" href="/">`) {
		t.Errorf("sign-in: %d, want the brand linking home:\n%s", page.Status, page.Body)
	}
}

// The docs for agents that run outside are the hosted edition's, so the
// open console's token page doesn't send anyone to them.
func TestTheOpenTokenPageSendsNobodyToDocsThatArentThere(t *testing.T) {
	w := NewWorld(t)
	_, hired, _ := HireOutside(t, w)

	if strings.Contains(hired.Body, "/docs/") {
		t.Errorf("the token page links to docs that aren't here:\n%s", hired.Text())
	}
	if !strings.Contains(hired.Text(), "acme."+BaseDomain+"/agents/v1") {
		t.Errorf("the token page doesn't say where the agent's API is:\n%s", hired.Text())
	}
}
