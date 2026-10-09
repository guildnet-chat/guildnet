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

// The console's pages show what's there first. A form for adding something
// (an agent, an invite, a team, a goal) waits behind a button at the top of
// the page, and opens in place when pressed, or when what was sent needs
// fixing.
func TestAddingFormsOpenWhenAskedFor(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	for _, c := range []struct{ page, form, opener string }{
		{"/", `action="/staff"`, `href="/?add=1#hire"`},
		{"/people", `action="/people/invite"`, `href="/people?add=1#add"`},
		{"/teams", `action="/teams"`, `href="/teams?add=1#add"`},
		{"/teams/Engineering", `action="/goals"`, `href="/teams/Engineering?add=1#add"`},
		{"/company", `action="/goals"`, `href="/company?add=1#add"`},
	} {
		closed := ada.Get(c.page)
		if strings.Contains(closed.Body, c.form) {
			t.Errorf("%s shows its adding form before it's asked for", c.page)
		}
		if !strings.Contains(closed.Body, c.opener) {
			t.Errorf("%s should have a button that opens its adding form (%s)", c.page, c.opener)
		}
		sep := "?"
		if strings.Contains(c.page, "?") {
			sep = "&"
		}
		if open := ada.Get(c.page + sep + "add=1"); !strings.Contains(open.Body, c.form) {
			t.Errorf("%s?add=1 should show its adding form", c.page)
		}
	}

	// Starting from a role in the catalogue opens the hiring form with it.
	if r := ada.Get("/?preset=software-engineer"); !strings.Contains(r.Body, `action="/staff"`) {
		t.Error("hiring from the role catalogue should open the hiring form")
	}
	// A form sent with something to fix stays open, to fix it.
	ada.Get("/?add=1")
	r := ada.Post("/staff", url.Values{"nick": {"rex"}, "budget": {"lots"}})
	if !strings.Contains(r.Body, `action="/staff"`) || !strings.Contains(r.Text(), "Budget is dollars per day") {
		t.Errorf("a hire to fix should come back with the form open: %d", r.Status)
	}
}

// What's waiting on you leads the org chart, a few at a time, with the rest
// a click away; it no longer takes a column of its own beside the page.
func TestApprovalsLeadTheOrgChart(t *testing.T) {
	w := NewWorld(t)
	for _, s := range []string{"merge PR #1 into main", "merge PR #2 into main", "merge PR #3 into main", "merge PR #4 into main"} {
		w.Networks["acme"].RaiseApproval(t, "kai", "ada", s)
	}

	r := w.Browser("acme").SignedIn("ada").Get("/")

	if strings.Contains(r.Body, `<aside class="waiting"`) {
		t.Error("approvals shouldn't take a column beside the org chart any more")
	}
	if n := strings.Count(r.Body, `/approve"`); n != 3 {
		t.Errorf("the org chart shows %d approvals to decide, want the first 3", n)
	}
	if !strings.Contains(r.Text(), "1 more") || !strings.Contains(r.Body, `href="/approvals"`) {
		t.Error("the org chart should say how many more are waiting, and link to them")
	}
	if strings.Index(r.Body, "<h1") > strings.Index(r.Body, `/approve"`) {
		t.Error("the page's title should come before what's waiting")
	}
}

// A page inside another says where it is, and links back up.
func TestPagesInsideOthersSayWhereTheyAre(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	for path, up := range map[string]string{
		"/teams/Engineering": `<a href="/teams">Teams</a>`,
		"/people/bo":         `<a href="/people">People</a>`,
		"/access":            `<a href="/people">People</a>`,
		"/staff/kai":         `<a href="/">Org chart</a>`,
		"/roles":             `<a href="/">Org chart</a>`,
	} {
		r := ada.Get(path)
		crumbs := regexp.MustCompile(`(?s)<nav class="crumbs" aria-label="Breadcrumb">(.*?)</nav>`).FindStringSubmatch(r.Body)
		if crumbs == nil || !strings.Contains(crumbs[1], up) {
			t.Errorf("%s should lead back up with %s", path, up)
		}
	}
}

// Sums of money read the way people write them.
func TestMoneyHasThousandsSeparators(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	for _, path := range []string{"/teams", "/teams/Engineering"} {
		text := ada.Get(path).Text()
		if !strings.Contains(text, "$20,000/month") || strings.Contains(text, "$20000") {
			t.Errorf("%s should show Engineering's budget as $20,000/month", path)
		}
	}
}

// Nothing to show reads as nothing yet, not as being kept out.
func TestAnEmptyLogSaysSo(t *testing.T) {
	w := NewWorld(t)

	text := w.Browser("acme").SignedIn("ada").Get("/log").Text()

	if !strings.Contains(text, "No changes yet.") || strings.Contains(text, "that you may see") {
		t.Errorf("the owner's empty log should say there are no changes yet:\n%s", text)
	}
}

// Back and Forward between console pages bring back the page, and the
// sidebar with it: htmx's history restores the element marked
// hx-history-elt (without it, it swaps the whole body for just the page,
// losing the sidebar and the frame). Checked in a browser with
// scratchpad back.mjs; here, that the mark stays.
func TestBackAndForwardRestoreTheConsolesPage(t *testing.T) {
	w := NewWorld(t)

	r := w.Browser("acme").SignedIn("ada").Get("/teams")

	if !regexp.MustCompile(`<div id="page" class="page" hx-history-elt`).MatchString(r.Body) {
		t.Error("the console's page should be what Back and Forward restore")
	}
}

// An address that isn't a page gets a page that says so, in the console's
// own look, with a way back.
func TestAMissingPageIsAPage(t *testing.T) {
	w := NewWorld(t)

	for _, b := range []*Browser{w.Browser("acme").SignedIn("ada"), w.Home()} {
		r := b.Get("/no-such-page")
		if r.Status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", b.Host, r.Status)
		}
		if !strings.Contains(r.Body, "/static/tokens.css") || !strings.Contains(r.Text(), "Page not found") || !strings.Contains(r.Body, `href="/"`) {
			t.Errorf("%s's missing page should be a styled page with a way back:\n%s", b.Host, r.Body)
		}
	}
}
