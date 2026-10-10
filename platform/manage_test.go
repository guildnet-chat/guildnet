// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/org"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Managing the org from the console: everything GuildServ can do, in forms,
// for whoever the org's rules allow. The console offers only what will
// work; the runner checks every change anyway.

// acme: ada owns it; vera (an agent) leads Engineering, with kai and cy;
// bo leads Sales.

func TestALeadInvitesPeopleIntoTheirOwnTeamsOnly(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")

	page := bo.Get("/people?add=1")
	if !strings.Contains(page.Body, `action="/people/invite"`) {
		t.Fatalf("bo leads Sales, so should be offered inviting:\n%s", page.Text())
	}
	if strings.Contains(page.Body, "<option>Engineering</option>") || strings.Contains(page.Body, `<option value="">No team</option>`) {
		t.Error("bo should only be offered Sales to invite into")
	}
	if r := bo.Post("/people/invite", url.Values{"role": {"Engineer"}, "team": {"Engineering"}}); r.Status != http.StatusForbidden || JoinLink.MatchString(r.Body) {
		t.Errorf("bo inviting into Engineering: %d, want 403 and no link", r.Status)
	}
	bo.Get("/people")
	r := bo.Post("/people/invite", url.Values{"role": {"Seller"}, "team": {"Sales"}})
	link := JoinLink.FindString(r.Body)
	if r.Status != http.StatusOK || link == "" {
		t.Fatalf("bo inviting into Sales: %d %s", r.Status, r.Text())
	}
	dee := w.Browser("acme")
	dee.Get(link)
	if r := dee.Post(link, url.Values{"nick": {"dee"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("joining: %d %s", r.Status, r.Text())
	}
	if m, err := w.Networks["acme"].Store.Member(context.Background(), "dee"); err != nil || m.Team != "Sales" || m.ReportsTo != "bo" {
		t.Errorf("dee in the org: %+v %v, want in Sales reporting to bo", m, err)
	}
}

// A reset link sets someone's IRC password, so whoever makes it could sign
// in as them: only those who may invite across the whole org make them.
func TestOnlyTheOwnerAndAdminsMakeResetLinks(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	w.Networks["acme"].Store.AddMember(context.Background(), org.Member{Account: "eli", Team: "Sales", ReportsTo: "bo"})

	bo.Get("/people")
	if r := bo.Post("/people/eli/reset", nil); r.Status != http.StatusForbidden || ResetLink.MatchString(r.Body) {
		t.Errorf("bo making eli's reset link: %d, want 403 and no link", r.Status)
	}
}

func TestAPersonsPageShowsTheirPlace(t *testing.T) {
	w := NewWorld(t)

	page := w.Browser("acme").SignedIn("bo").Get("/people/cy")

	if page.Status != http.StatusOK {
		t.Fatalf("cy's page: %d", page.Status)
	}
	for _, want := range []string{"Engineer", "Engineering", "vera"} {
		if !strings.Contains(page.Text(), want) {
			t.Errorf("cy's page should show %q:\n%s", want, page.Text())
		}
	}
	if strings.Contains(page.Body, `action="/people/cy"`) {
		t.Error("bo can't change cy, so shouldn't be offered to")
	}
}

func TestChangingSomeonesPlaceFromTheConsole(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	if page := ada.Get("/people/cy"); !strings.Contains(page.Body, `action="/people/cy"`) {
		t.Fatalf("ada should be offered changing cy:\n%s", page.Text())
	}

	r := ada.Post("/people/cy", url.Values{"role": {"Staff engineer"}, "team": {"Engineering"}, "reports_to": {"ada"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("saving cy: %d %s", r.Status, r.Text())
	}
	if m, _ := w.Networks["acme"].Store.Member(context.Background(), "cy"); m.Role != "Staff engineer" || m.ReportsTo != "ada" {
		t.Errorf("cy = %+v", m)
	}
	bo := w.Browser("acme").SignedIn("bo")
	bo.Get("/people/cy")
	if r := bo.Post("/people/cy", url.Values{"role": {"Intern"}}); r.Status != http.StatusForbidden {
		t.Errorf("bo changing cy: %d, want 403", r.Status)
	}
}

func TestGivingAccessFromTheConsole(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people/bo")

	if r := ada.Post("/people/bo/access", url.Values{"level": {"Admin"}, "give": {"1"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("giving bo Admin: %d %s", r.Status, r.Text())
	}
	if m, _ := w.Networks["acme"].Store.Member(context.Background(), "bo"); !slices.Contains(m.Access, "Admin") {
		t.Errorf("bo's access = %v, want Admin", m.Access)
	}
	if page := w.Browser("acme").SignedIn("bo").Get("/people/cy"); !strings.Contains(page.Body, `action="/people/cy"`) {
		t.Error("as an admin, bo should now be offered changing cy")
	}
}

func TestALeadMakesATeamInsideTheirs(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	page := bo.Get("/teams?add=1")
	if !strings.Contains(page.Body, `action="/teams"`) {
		t.Fatalf("bo should be offered making a team inside Sales:\n%s", page.Text())
	}

	r := bo.Post("/teams", url.Values{"name": {"Partners"}, "parent": {"Sales"}})

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/teams/Partners" {
		t.Fatalf("making Partners: %d %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if team, err := w.Networks["acme"].Store.Team(context.Background(), "Partners"); err != nil || team.Parent != "Sales" {
		t.Errorf("Partners = %+v %v, want inside Sales", team, err)
	}
	bo.Get("/teams")
	if r := bo.Post("/teams", url.Values{"name": {"Growth"}}); r.Status != http.StatusForbidden {
		t.Errorf("bo making a team at the top: %d, want 403", r.Status)
	}
}

func TestATeamsPageSetsItsFocusAndObjectives(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/teams/Sales"); page.Status != http.StatusOK || !strings.Contains(page.Body, `action="/teams/Sales"`) {
		t.Fatalf("Sales' page for its lead: %d\n%s", page.Status, page.Text())
	}

	if r := bo.Post("/teams/Sales", url.Values{"focus": {"Win bakeries that stay"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("setting the focus: %d %s", r.Status, r.Text())
	}
	bo.Get("/teams/Sales")
	if r := bo.Post("/goals", url.Values{"team": {"Sales"}, "text": {"Ten new bakeries a month"}, "measure": {"New bakeries"}, "target": {"10"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("adding an objective: %d %s", r.Status, r.Text())
	}

	page := bo.Get("/teams/Sales").Text()
	for _, want := range []string{"Win bakeries that stay", "Ten new bakeries a month", "New bakeries"} {
		if !strings.Contains(page, want) {
			t.Errorf("Sales' page should show %q:\n%s", want, page)
		}
	}
	if eng := bo.Get("/teams/Engineering"); strings.Contains(eng.Body, `action="/teams/Engineering"`) {
		t.Error("bo can't change Engineering, so shouldn't be offered to")
	}
}

func TestTheCompanyPage(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/company")

	if r := ada.Post("/company", url.Values{"description": {"We make ovens for small bakeries."}}); r.Status != http.StatusSeeOther {
		t.Fatalf("describing: %d %s", r.Status, r.Text())
	}
	ada.Get("/company")
	if r := ada.Post("/goals", url.Values{"text": {"Grow to 100 weekly customers"}, "current": {"40"}, "target": {"100"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("adding a goal: %d %s", r.Status, r.Text())
	}

	page := w.Browser("acme").SignedIn("cy").Get("/company")
	for _, want := range []string{"ovens for small bakeries", "Grow to 100 weekly customers", "40", "100"} {
		if !strings.Contains(page.Text(), want) {
			t.Errorf("the company page should show %q:\n%s", want, page.Text())
		}
	}
	if strings.Contains(page.Body, `action="/company"`) {
		t.Error("cy can't describe the company, so shouldn't be offered to")
	}
}

func TestTheAccessPage(t *testing.T) {
	w := NewWorld(t)
	cy := w.Browser("acme").SignedIn("cy")
	if page := cy.Get("/access"); page.Status != http.StatusOK || !strings.Contains(page.Text(), "Manager") || strings.Contains(page.Body, `action="/access/`) {
		t.Errorf("cy sees the access levels but can't change them: %d", page.Status)
	}
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/access")

	r := ada.Post("/access/Manager", url.Values{"abilities": {"approve", "spend", "hire"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("changing Manager: %d %s", r.Status, r.Text())
	}
	if page := ada.Get("/access"); !strings.Contains(page.Text(), "Manager: approve, spend, hire") {
		t.Errorf("Manager should now include hire:\n%s", page.Text())
	}
	cy.Get("/access")
	if r := cy.Post("/access/Manager", url.Values{"abilities": {"hire"}}); r.Status != http.StatusForbidden {
		t.Errorf("cy changing Manager: %d, want 403", r.Status)
	}
}

func TestTheLogShowsWhatYouMaySee(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/people/cy")
	ada.Post("/people/cy", url.Values{"role": {"Staff engineer"}})

	if page := ada.Get("/log"); !strings.Contains(page.Text(), "Staff engineer") {
		t.Errorf("ada's log should show cy's new title:\n%s", page.Text())
	}
	if page := w.Browser("acme").SignedIn("bo").Get("/log"); strings.Contains(page.Text(), "Staff engineer") {
		t.Errorf("bo leads Sales, and shouldn't see changes in Engineering:\n%s", page.Text())
	}
}

func TestAnAgentsManagerIsChosenOnItsPage(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	if page := ada.Get("/staff/kai"); !strings.Contains(page.Body, `name="reports_to"`) {
		t.Fatalf("kai's page should offer choosing its manager:\n%s", page.Text())
	}

	if r := ada.Post("/staff/kai", url.Values{"reports_to": {"cy"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("saving kai: %d %s", r.Status, r.Text())
	}
	if p, _ := w.Networks["acme"].Store.Agent(context.Background(), "kai"); p.ReportsTo != "cy" {
		t.Errorf("kai reports to %q, want cy", p.ReportsTo)
	}
}

// The org chart is the reporting tree: the owner at the top, everyone under
// their manager, with how many report to each. By team is the other view.
func TestTheOrgChartShowsWhoReportsToWhom(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	page := ada.Get("/")

	under := func(manager, report string) bool {
		return regexp.MustCompile(`data-person="` + manager + `"[\s\S]*?<ul[^>]*>[\s\S]*?data-person="` + report + `"`).MatchString(page.Body)
	}
	if !under("ada", "vera") || !under("vera", "kai") || !under("vera", "cy") || !under("ada", "bo") {
		t.Errorf("the tree should have vera and bo under ada, and kai and cy under vera:\n%s", page.Text())
	}
	if !strings.Contains(page.Text(), "2 reports") {
		t.Error("vera's card should say she has 2 reports")
	}
	picture := ada.Get("/?view=picture")
	if picture.Status != http.StatusOK || !strings.Contains(picture.Body, `class="org-picture"`) {
		t.Errorf("the picture: %d, want the drawn chart", picture.Status)
	}
	for _, who := range []string{"ada", "vera", "kai", "cy", "bo"} {
		if !strings.Contains(picture.Body, `data-person="`+who+`"`) {
			t.Errorf("the picture should have %s on it", who)
		}
	}
	byTeam := ada.Get("/?view=teams")
	if byTeam.Status != http.StatusOK || !strings.Contains(byTeam.Body, `class="card team"`) {
		t.Errorf("by team: %d, want the team cards", byTeam.Status)
	}
}

// The owner sets how long an approval waits on one person before moving on;
// each approval says who's next and when.
func TestTheOwnerSetsHowLongApprovalsWait(t *testing.T) {
	w := NewWorld(t)
	n := w.Networks["acme"]
	ada := w.Browser("acme").SignedIn("ada")
	if page := ada.Get("/approvals"); !strings.Contains(page.Body, `action="/approvals/after"`) {
		t.Fatalf("the owner's approvals page should offer the setting:\n%s", page.Text())
	}

	if r := ada.Post("/approvals/after", url.Values{"after": {"1h"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("setting it: %d %s", r.Status, r.Text())
	}
	if v, _ := n.Store.Setting(context.Background(), "approvals_move_after"); v != "1h" {
		t.Errorf("setting = %q, want 1h", v)
	}
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/approvals"); strings.Contains(page.Body, `action="/approvals/after"`) {
		t.Error("only the owner should be offered the setting")
	}
	bo.Get("/approvals")
	if r := bo.Post("/approvals/after", url.Values{"after": {"5m"}}); r.Status != http.StatusForbidden {
		t.Errorf("bo setting it: %d, want 403", r.Status)
	}
}

// The role catalogue: a page of roles by department, and hiring from one.
func TestHiringFromTheRoleCatalogue(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	page := ada.Get("/roles")
	for _, want := range []string{"Support", "Support agent", "Bookkeeper", "asks first"} {
		if !strings.Contains(page.Text(), want) {
			t.Errorf("the catalogue should show %q", want)
		}
	}
	if form := ada.Get("/?preset=support-agent"); !strings.Contains(form.Body, `<option value="support-agent" selected>`) {
		t.Error("hiring from the catalogue should start the form from that role")
	}
	r := ada.Post("/staff", url.Values{"nick": {"pip"}, "preset": {"support-agent"}, "team": {"Sales"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("hiring pip: %d %s", r.Status, r.Text())
	}
	if p, _ := w.Networks["acme"].Store.Agent(context.Background(), "pip"); p.Role != "Support agent" || p.Model != "claude-haiku-4-5" {
		t.Errorf("pip = %+v, want a support agent", p)
	}
}

// A team's page has its numbers: people, agents, approvals, and spend for
// those who may see it.
func TestATeamsPageHasItsNumbers(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].Ledger.RecordCost("kai", 1.25)

	byAda := w.Browser("acme").SignedIn("ada").Get("/teams/Engineering")
	byBo := w.Browser("acme").SignedIn("bo").Get("/teams/Engineering")

	for _, want := range []string{"Agents", "Approvals waiting", "Spent this month", "$1.25"} {
		if !strings.Contains(byAda.Text(), want) {
			t.Errorf("Engineering's page for the owner should show %q", want)
		}
	}
	if strings.Contains(byBo.Text(), "$1.25") {
		t.Error("bo leads Sales, and mustn't see what Engineering spent")
	}
}

// A team's leads choose its repos, from those GitHub gives the network.
func TestATeamsLeadChoosesItsReposInTheConsole(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	page := bo.Get("/teams/Sales")
	if !strings.Contains(page.Body, `name="repos" value="web"`) || !strings.Contains(page.Body, `name="repos" value="docs"`) {
		t.Fatalf("Sales' page should offer the network's repos:\n%s", page.Text())
	}

	if r := bo.Post("/teams/Sales", url.Values{"repos": {"", "docs"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("choosing docs: %d %s", r.Status, r.Text())
	}
	if team, _ := w.Networks["acme"].Store.Team(context.Background(), "Sales"); !slices.Equal(team.Repos, []string{"docs"}) {
		t.Errorf("Sales' repos = %v, want docs", team.Repos)
	}
}

// Hiring offers a desk (a workspace with tools) only where the network can
// run them; nobody's moved onto one without choosing it.
func TestHiringOffersADeskOnlyWhereThereAreDesks(t *testing.T) {
	w := NewWorld(t)
	page := w.Browser("acme").SignedIn("ada").Get("/")

	if strings.Contains(page.Body, `<option value="managed"`) || strings.Contains(page.Body, `<option value="desk"`) {
		t.Error("acme's agents can't have desks (no sandbox or Managed Agents here), so hiring shouldn't offer one")
	}
	if staff := w.Browser("acme").SignedIn("ada").Get("/staff/kai"); strings.Contains(staff.Body, `<option value="desk"`) || strings.Contains(staff.Body, `<option value="managed"`) {
		t.Error("nor should an agent's page")
	}
}

// Desks of GuildNet's own (any model) are offered where the network has a
// sandbox, apart from Managed Agents' (Claude only).
func TestHiringOffersADeskOfGuildNetsOwnWhereThereIsASandbox(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].Brain.Offer("desk")
	ada := w.Browser("acme").SignedIn("ada")

	hire := ada.Get("/?add=1")
	staff := ada.Get("/staff/kai")

	if !strings.Contains(hire.Body, `<option value="desk"`) || strings.Contains(hire.Body, `<option value="managed"`) {
		t.Errorf("hiring should offer a desk of GuildNet's own, and not Managed Agents':\n%s", hire.Text())
	}
	if !strings.Contains(staff.Body, `<option value="desk"`) || !strings.Contains(staff.Text(), "any model") {
		t.Errorf("kai's page should offer moving to a desk:\n%s", staff.Text())
	}
}

// A team's leads choose how much of its agents' work the team's channel
// sees.
func TestATeamsLeadChoosesHowChattyItsChannelIs(t *testing.T) {
	w := NewWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/teams/Sales"); !strings.Contains(page.Body, `name="chatter"`) {
		t.Fatalf("Sales' page should offer the setting:\n%s", page.Text())
	}

	if r := bo.Post("/teams/Sales", url.Values{"chatter": {"quiet"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("choosing quiet: %d %s", r.Status, r.Text())
	}
	if team, _ := w.Networks["acme"].Store.Team(context.Background(), "Sales"); team.Chatter != "quiet" {
		t.Errorf("Sales' chatter = %q, want quiet", team.Chatter)
	}
	if page := bo.Get("/teams/Sales"); !strings.Contains(page.Body, `value="quiet" selected`) {
		t.Error("the page should show it's quiet now")
	}
}

// An agent's page shows its recent work, to those who may see it: each
// wake-up with where, for whom, what it achieved, and its steps.
func TestAnAgentsPageShowsItsRecentWork(t *testing.T) {
	w := NewWorld(t)
	at := time.Now().Add(-10 * time.Minute).UTC()
	w.Networks["acme"].Store.AddWork(context.Background(), org.Work{Agent: "kai", Team: "Engineering", Where: "#engineering",
		Asker: "cy", Asked: "fix the Safari login", Started: at, Finished: at.Add(4 * time.Minute), Outcome: "answered", Cost: 0.07,
		Steps: []org.WorkStep{{At: at.Add(10 * time.Second), Summary: "reads login.go"}, {At: at.Add(3 * time.Minute), Summary: "opened PR #12", Result: true}}})
	ada := w.Browser("acme").SignedIn("ada")

	page := ada.Get("/staff/kai").Text()

	for _, want := range []string{"Recent work", "#engineering, for cy", "fix the Safari login", "4m 0s", "reads login.go", "opened PR #12", "+10s", "$0.07"} {
		if !strings.Contains(page, want) {
			t.Errorf("kai's page should show %q:\n%s", want, page)
		}
	}
	if page := w.Browser("acme").SignedIn("bo").Get("/staff/kai").Text(); strings.Contains(page, "Recent work") {
		t.Error("bo, outside Engineering, shouldn't see kai's work")
	}
}
