// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Who may do what, and where. The owner can do anything. Everyone else gets
// abilities (approve, hire, budgets, invite, arrange, spend, log) from their
// access levels: Lead for the teams they lead, Manager for everyone below
// them, Member for their own team, and any the owner gives them (Admin, or
// levels the owner makes). Each works only within its scope, and nobody can
// act on themselves or anyone above them, or give more than they hold.

// salesOrg sets up, as the owner, through the API:
//
//	Sales ($10000/month), led by dana, with eli in it
//	  EMEA ($4000/month), inside Sales, with eve in it (costing $3000)
//	Engineering (from the seed, led by vera), with sam in it
func salesOrg(t *testing.T) *network {
	t.Helper()
	n := startNetwork(t, &fakeBrain{}, withAPI())
	owner := n.api("method")
	must := func(method, path string, body map[string]any) {
		t.Helper()
		if status, out := owner.call(method, path, body); status >= 300 {
			t.Fatalf("setting up, %s %s: %d %v", method, path, status, out)
		}
	}
	must("POST", "/v1/teams", map[string]any{"name": "Sales", "budget": 10000})
	must("POST", "/v1/members", map[string]any{"account": "dana", "role": "Head of Sales", "team": "Sales"})
	must("POST", "/v1/members", map[string]any{"account": "eli", "role": "Account exec", "team": "Sales"})
	must("POST", "/v1/people/dana/promote", nil)
	must("POST", "/v1/teams", map[string]any{"name": "EMEA", "parent": "Sales", "budget": 4000})
	must("POST", "/v1/members", map[string]any{"account": "eve", "role": "Account exec", "team": "EMEA", "cost": 3000})
	must("POST", "/v1/members", map[string]any{"account": "sam", "role": "Engineer", "team": "Engineering"})
	return n
}

// expectStatus calls the API as actor and checks the status.
func expectStatus(t *testing.T, n *network, actor string, want int, method, path string, body map[string]any) map[string]any {
	t.Helper()
	status, out := n.api(actor).call(method, path, body)
	if status != want {
		t.Errorf("%s %s %s: %d %v, want %d", actor, method, path, status, out, want)
	}
	return out
}

func teamIn(t *testing.T, n *network, name string) map[string]any {
	t.Helper()
	_, out := n.api("method").call("GET", "/v1/org", nil)
	for _, x := range out["teams"].([]any) {
		if team := x.(map[string]any); team["name"] == name {
			return team
		}
	}
	t.Fatalf("no team %s in %v", name, out["teams"])
	return nil
}

func strs(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

func TestATeamCanHaveSeveralLeads(t *testing.T) {
	n := salesOrg(t)

	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eli/promote", nil)

	if leads := strs(teamIn(t, n, "Sales")["leads"]); !slices.Equal(leads, []string{"dana", "eli"}) {
		t.Errorf("Sales' leads = %v, want dana and eli", leads)
	}
	expectStatus(t, n, "eli", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "rex", "team": "Sales", "budget": 1})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eli/demote", nil)
	if leads := strs(teamIn(t, n, "Sales")["leads"]); !slices.Equal(leads, []string{"dana"}) {
		t.Errorf("after demoting eli, Sales' leads = %v, want just dana", leads)
	}
}

func TestALeadRunsTheTeamsInsideTheirs(t *testing.T) {
	n := salesOrg(t)

	expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "rex", "team": "EMEA", "budget": 1})
	expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/teams", map[string]any{"name": "APAC", "parent": "Sales"})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/teams", map[string]any{"name": "Growth"})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/staff", map[string]any{"nick": "bot", "team": "Engineering", "budget": 1})
	if parent := teamIn(t, n, "APAC")["parent"]; parent != "Sales" {
		t.Errorf("APAC's parent = %v, want Sales", parent)
	}
}

func TestALeadOfAnInnerTeamCantReachOutOfIt(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eve/promote", nil)

	expectStatus(t, n, "eve", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "rex", "team": "EMEA", "budget": 1})
	expectStatus(t, n, "eve", http.StatusForbidden, "POST", "/v1/staff", map[string]any{"nick": "bot", "team": "Sales", "budget": 1})
}

func TestALeadInvitesAndArrangesPeopleInTheirTeams(t *testing.T) {
	n := salesOrg(t)

	expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/members", map[string]any{"account": "fay", "team": "EMEA"})
	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/members/eve", map[string]any{"role": "Senior account exec"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/members/eve", map[string]any{"team": "Engineering"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/members/sam", map[string]any{"role": "Salesperson"})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/members", map[string]any{"account": "gil", "team": "Engineering"})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/members", map[string]any{"account": "gil"})
}

func TestNobodyChangesThemselvesOrAnyoneAboveThem(t *testing.T) {
	n := salesOrg(t)
	// eve reports to dana; make dana report to eli, who is in Sales too.
	expectStatus(t, n, "method", http.StatusOK, "PATCH", "/v1/members/dana", map[string]any{"reports_to": "eli"})

	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/members/dana", map[string]any{"role": "CEO"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/members/eli", map[string]any{"role": "Intern"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/members/method", map[string]any{"role": "Intern"})
	expectStatus(t, n, "dana", http.StatusForbidden, "DELETE", "/v1/members/dana", nil)
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/people/dana/demote", nil)
}

func TestTeamBudgetsNestInsideTheTeamAbove(t *testing.T) {
	n := salesOrg(t)

	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/teams/EMEA", map[string]any{"budget": 6000})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/teams/EMEA", map[string]any{"budget": 50000})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/teams/Sales", map[string]any{"budget": 99999})
	if b := teamIn(t, n, "EMEA")["monthly_budget"]; b != 6000.0 {
		t.Errorf("EMEA's budget = %v, want 6000", b)
	}
}

func TestATeamCantEndUpInsideItself(t *testing.T) {
	n := salesOrg(t)

	out := expectStatus(t, n, "method", http.StatusBadRequest, "PATCH", "/v1/teams/Sales", map[string]any{"parent": "EMEA"})
	if !strings.Contains(errorOf(out), "inside") {
		t.Errorf("refusal = %q, want it to say why", errorOf(out))
	}
}

func TestTheOwnerDecidesWhatManagersMayDo(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "bot", "team": "Engineering", "reports_to": "sam"})

	expectStatus(t, n, "sam", http.StatusForbidden, "PATCH", "/v1/staff/bot", map[string]any{"role": "Build bot"})
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Manager", map[string]any{"abilities": []string{"approve", "spend", "hire"}})
	expectStatus(t, n, "sam", http.StatusOK, "PATCH", "/v1/staff/bot", map[string]any{"role": "Build bot"})
	expectStatus(t, n, "sam", http.StatusForbidden, "PATCH", "/v1/staff/kai", map[string]any{"role": "Build bot"})
}

func TestAnAdminRunsTheOrgButNotWhoMayDoWhat(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/dana/access/Admin", nil)

	expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/teams", map[string]any{"name": "Growth"})
	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/members/sam", map[string]any{"role": "Staff engineer"})
	expectStatus(t, n, "dana", http.StatusForbidden, "PUT", "/v1/access/Ops", map[string]any{"abilities": []string{"spend"}, "scope": "org"})
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/people/sam/access/Admin", nil)
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/teams/Sales", map[string]any{"budget": 99999})
}

func TestTheOwnerMakesTheirOwnAccessLevels(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Ops", map[string]any{"abilities": []string{"spend", "log"}, "scope": "org"})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/sam/access/Ops", nil)

	if out := expectStatus(t, n, "sam", http.StatusOK, "GET", "/v1/members/eve", nil); out["monthly_cost"] != 3000.0 {
		t.Errorf("sam, with Ops, should see what eve costs: %v", out)
	}
	expectStatus(t, n, "sam", http.StatusForbidden, "POST", "/v1/staff", map[string]any{"nick": "bot", "team": "Engineering"})
	_, out := n.api("sam").call("GET", "/v1/access", nil)
	var names []string
	for _, x := range out["levels"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"Admin", "Lead", "Manager", "Member", "Ops"} {
		if !slices.Contains(names, want) {
			t.Errorf("access levels = %v, want %s among them", names, want)
		}
	}
}

func TestYouCantGiveMoreThanYouHold(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Sales helper", map[string]any{"abilities": []string{"invite"}, "scope": "teams", "teams": []string{"Sales"}})
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Recruiter", map[string]any{"abilities": []string{"invite"}, "scope": "org"})

	expectStatus(t, n, "dana", http.StatusOK, "POST", "/v1/people/eve/access/Sales helper", nil)
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/people/eve/access/Recruiter", nil)
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/people/sam/access/Sales helper", nil)
	expectStatus(t, n, "dana", http.StatusOK, "DELETE", "/v1/people/eve/access/Sales helper", nil)
}

// A level that reaches nothing yet (the teams someone leads, when they lead
// none) would reach whatever they're given later, by whoever gives it: so
// only someone holding it across the org may give it.
func TestALevelThatReachesNothingYetIsntGivenFromBelow(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Recruiter", map[string]any{"abilities": []string{"invite"}, "scope": "led"})

	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/people/eve/access/Recruiter", nil)
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eve/promote", nil)
	expectStatus(t, n, "dana", http.StatusOK, "POST", "/v1/people/eve/access/Recruiter", nil)
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eli/access/Recruiter", nil)
}

// A preset brings its own budget: it changes the budget like BUDGET does.
func TestAPresetCountsAsSettingTheBudget(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Hirer", map[string]any{"abilities": []string{"hire"}, "scope": "teams", "teams": []string{"Sales"}})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eli/access/Hirer", nil)
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "rex", "team": "Sales", "budget": 1})

	expectStatus(t, n, "eli", http.StatusForbidden, "PATCH", "/v1/staff/rex", map[string]any{"preset": "software-engineer"})
	expectStatus(t, n, "eli", http.StatusOK, "PATCH", "/v1/staff/rex", map[string]any{"role": "Closer"})
	if out := expectStatus(t, n, "method", http.StatusOK, "GET", "/v1/staff/rex", nil); out["budget"] != 1.0 {
		t.Errorf("rex's budget = %v, want the $1 it had", out["budget"])
	}
}

func TestAPresetCantTakeATeamPastItsBudget(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "PATCH", "/v1/members/eve", map[string]any{"cost": 3990})
	expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "rex", "team": "EMEA", "budget": 0.1})

	out := expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/staff/rex", map[string]any{"preset": "software-engineer"})
	if !strings.Contains(errorOf(out), "left") {
		t.Errorf("refusal = %q, want it to say what EMEA has left", errorOf(out))
	}
}

// An agent's manager may approve what it does, so whoever picks the manager
// must hold that already: hiring, or naming yourself, doesn't hand it out.
func TestHiringDoesntMakeYouTheAgentsManager(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/teams", map[string]any{"name": "Support", "budget": 1000})
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/members", map[string]any{"account": "sue", "team": "Support"})
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Hirer", map[string]any{"abilities": []string{"hire"}, "scope": "own"})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/sue/access/Hirer", nil)

	expectStatus(t, n, "sue", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "bot", "team": "Support"})
	if out := expectStatus(t, n, "method", http.StatusOK, "GET", "/v1/staff/bot", nil); out["reports_to"] != "method" {
		t.Errorf("bot reports to %v, want the owner: sue can't approve for it", out["reports_to"])
	}
	expectStatus(t, n, "sue", http.StatusForbidden, "PATCH", "/v1/staff/bot", map[string]any{"reports_to": "sue"})
	expectStatus(t, n, "sue", http.StatusForbidden, "POST", "/v1/staff", map[string]any{"nick": "bot2", "team": "Support", "reports_to": "sue"})
	expectStatus(t, n, "sue", http.StatusForbidden, "POST", "/v1/staff", map[string]any{"nick": "bot3", "team": "Support", "reports_to": "eli"})
	expectStatus(t, n, "sue", http.StatusOK, "PATCH", "/v1/staff/bot", map[string]any{"reports_to": "method", "role": "Helpdesk"})
}

// The same holds for people: whoever someone reports to manages the agents
// below them too.
func TestYouCantMakePeopleReportToYouWithoutHoldingWhatManagersDo(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusOK, "PUT", "/v1/access/Arranger", map[string]any{"abilities": []string{"arrange"}, "scope": "teams", "teams": []string{"Engineering"}})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/people/eli/access/Arranger", nil)
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "bot", "team": "Engineering", "reports_to": "sam"})

	expectStatus(t, n, "eli", http.StatusForbidden, "PATCH", "/v1/members/sam", map[string]any{"reports_to": "eli"})
	expectStatus(t, n, "eli", http.StatusOK, "PATCH", "/v1/members/sam", map[string]any{"role": "Staff engineer", "reports_to": "vera"})
	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/members/eve", map[string]any{"reports_to": "eli"})
}

// A guest isn't shown the company's directory through the API either: only
// who they are, their channels and the agents in them, and GuildServ (which
// gives guests only HELP and CATCHUP).
func TestTheAPIShowsAGuestOnlyTheirChannelsAndTheAgentsInThem(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/members", map[string]any{"account": "gus", "team": "Sales", "guest": true})

	for _, path := range []string{"/v1/org", "/v1/staff/kai", "/v1/staff/kai/work", "/v1/teams/Sales/budget", "/v1/teams/Sales/stats",
		"/v1/members/dana", "/v1/company", "/v1/access", "/v1/log", "/v1/approvals", "/v1/spend", "/v1/roles", "/v1/models",
		"/v1/connectors", "/v1/desk-hosts", "/v1/github"} {
		expectStatus(t, n, "gus", http.StatusForbidden, "GET", path, nil)
	}
	expectStatus(t, n, "gus", http.StatusForbidden, "POST", "/v1/goals", map[string]any{"text": "Ours now"})

	if me := expectStatus(t, n, "gus", http.StatusOK, "GET", "/v1/me", nil); me["guest"] != true {
		t.Errorf("/v1/me for a guest = %v, want guest: true", me)
	}
	mine := expectStatus(t, n, "gus", http.StatusOK, "GET", "/v1/me/channels", nil)
	var agents []string
	for _, x := range mine["agents"].([]any) {
		a := x.(map[string]any)
		agents = append(agents, a["nick"].(string))
		if a["role"] == "" {
			t.Errorf("agent %v has no role", a)
		}
		for _, ch := range strs(a["channels"]) {
			if !slices.Contains(strs(mine["channels"]), ch) {
				t.Errorf("%s is shown in %s, which isn't one of gus's channels %v", a["nick"], ch, mine["channels"])
			}
		}
	}
	if !slices.Equal(agents, []string{"kai", "vera"}) {
		t.Errorf("agents in gus's channels = %v, want kai and vera (in #general)", agents)
	}
	expectStatus(t, n, "method", http.StatusOK, "GET", "/v1/org", nil)
}

func TestGuestsCantBeGivenAccessOrLead(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "method", http.StatusCreated, "POST", "/v1/members", map[string]any{"account": "gus", "team": "Sales", "guest": true})

	expectStatus(t, n, "method", http.StatusBadRequest, "POST", "/v1/people/gus/access/Admin", nil)
	expectStatus(t, n, "method", http.StatusBadRequest, "POST", "/v1/people/gus/promote", nil)
}

func TestWhatYouMayDoIsSpelledOut(t *testing.T) {
	n := salesOrg(t)

	out := expectStatus(t, n, "dana", http.StatusOK, "GET", "/v1/me", nil)

	hire, _ := out["abilities"].(map[string]any)["hire"].(map[string]any)
	if teams := strs(hire["teams"]); !slices.Contains(teams, "Sales") || !slices.Contains(teams, "EMEA") || slices.Contains(teams, "Engineering") {
		t.Errorf("dana may hire in %v, want Sales and EMEA but not Engineering", teams)
	}
	if hire["org"] == true {
		t.Error("dana can't hire at the org level")
	}
	if people := strs(hire["people"]); !slices.Contains(people, "eve") || slices.Contains(people, "sam") || slices.Contains(people, "dana") {
		t.Errorf("dana may change %v, want eve but not sam or herself", people)
	}
	if !slices.Contains(strs(out["access"]), "Lead") {
		t.Errorf("dana's access = %v, want Lead", out["access"])
	}
	if owner := expectStatus(t, n, "method", http.StatusOK, "GET", "/v1/me", nil); owner["owner"] != true {
		t.Errorf("the owner's /v1/me = %v", owner)
	}
}

func TestTheLogShowsEachPersonTheChangesTheyMaySee(t *testing.T) {
	n := salesOrg(t)
	expectStatus(t, n, "dana", http.StatusCreated, "POST", "/v1/staff", map[string]any{"nick": "rex", "team": "EMEA", "budget": 1})
	expectStatus(t, n, "method", http.StatusOK, "PATCH", "/v1/members/sam", map[string]any{"role": "Staff engineer"})

	actions := func(actor string) []string {
		t.Helper()
		out := expectStatus(t, n, actor, http.StatusOK, "GET", "/v1/log", nil)
		var got []string
		for _, x := range out["entries"].([]any) {
			e := x.(map[string]any)
			got = append(got, e["actor"].(string)+" "+e["action"].(string)+" "+e["target"].(string))
		}
		return got
	}
	byOwner, byDana, bySam := actions("method"), actions("dana"), actions("sam")

	if !slices.Contains(byOwner, "dana hire rex") || !slices.Contains(byOwner, "method set member sam") {
		t.Errorf("the owner's log = %v, want both changes", byOwner)
	}
	if !slices.Contains(byDana, "dana hire rex") || slices.Contains(byDana, "method set member sam") {
		t.Errorf("dana's log = %v, want her hire but not sam's change in Engineering", byDana)
	}
	if len(bySam) != 0 {
		t.Errorf("sam may see no log, but got %v", bySam)
	}
}

// GuildServ holds to the same rules.
func TestLeadsManageTheirTeamsFromIRCToo(t *testing.T) {
	n := salesOrg(t)
	dana := n.joinAs("dana", "#general")

	dana.say("GuildServ", `MEMBER ADD fay ROLE "SDR" TEAM EMEA`)
	dana.expect("fay added", guildServSaying("dana", "added fay"))
	dana.say("GuildServ", "TEAM CREATE APAC PARENT Sales")
	dana.expect("APAC created", guildServSaying("dana", "created team APAC"))
	dana.say("GuildServ", "TEAM CREATE Growth")
	dana.expect("a refusal", guildServSaying("dana", "can't"))
	dana.say("GuildServ", "ACCESS")
	dana.expect("the access levels", guildServSaying("dana", "Manager"))
}

// The log says what changed, and only that: a form sends every field, and
// the ones it didn't change aren't news.
func TestTheLogOnlySaysWhatChanged(t *testing.T) {
	n := salesOrg(t)
	owner := n.api("method")
	owner.call("PATCH", "/v1/teams/Sales", map[string]any{"focus": "", "parent": "", "budget": 12000})
	kai, _ := n.store.Agent(t.Context(), "kai")
	owner.call("PATCH", "/v1/staff/kai", map[string]any{"prompt": kai.Prompt, "role": "Staff engineer"})
	owner.call("PATCH", "/v1/members/eve", map[string]any{"cost": 3500, "role": "Account exec"})

	_, out := owner.call("GET", "/v1/log?limit=3", nil)
	details := map[string]string{}
	for _, x := range out["entries"].([]any) {
		e := x.(map[string]any)
		details[e["action"].(string)+" "+e["target"].(string)], _ = e["detail"].(string)
	}
	if got := details["set team Sales"]; got != "budget $10000.00/month → $12000.00/month" {
		t.Errorf("Sales' change = %q, want just the budget", got)
	}
	if got := details["set agent kai"]; got != "role Backend engineer → Staff engineer" {
		t.Errorf("kai's change = %q, want just the role (the prompt was sent unchanged)", got)
	}
	if got := details["set member eve"]; got != "cost changed" {
		t.Errorf("eve's change = %q, want her cost said to change, but not what it is", got)
	}
}
