// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/ircfake"
	"github.com/guildnet-chat/guildnet/runner"
)

// The runner's internal API is how the console manages a network. It holds
// to the same rules as GuildServ, and it's safe to expose to the platform
// only: every call needs this network's secret and names the person acting,
// who must be in the org.

const networkSecret = "network-a-secret"

func withAPI() option {
	return func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.API.Listen = "127.0.0.1:0"
		cfg.API.Token = networkSecret
	}
}

// apiCaller calls a network's API as someone.
type apiCaller struct {
	t      *testing.T
	base   string
	token  string
	actor  string
	header bool // send the actor header at all
}

func (n *network) api(actor string) *apiCaller {
	return &apiCaller{t: n.t, base: "http://" + n.runner.APIAddr(), token: networkSecret, actor: actor, header: true}
}

// call makes a request and returns the status and the decoded JSON body.
func (c *apiCaller) call(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, in)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.header {
		req.Header.Set("GuildNet-Actor", c.actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errorOf(out map[string]any) string {
	s, _ := out["error"].(string)
	return s
}

// withSales sets up a Sales team led by dana, a human member.
func withSales(t *testing.T, n *network) {
	t.Helper()
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Sales`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "PROMOTE dana")
	n.human.expect("dana leading Sales", guildServSaying("method", "dana is now lead of Sales"))
	n.human.say("GuildServ", "TEAM SET Sales BUDGET 300")
	n.human.expect("Sales has a budget", guildServSaying("method", "$300"))
}

// Security.

func TestTheAPIRequiresItsNetworksSecret(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	for _, token := range []string{"", "network-b-secret", networkSecret + "x"} {
		c := n.api("method")
		c.token = token
		if status, _ := c.call("GET", "/v1/org", nil); status != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, status)
		}
	}
	if status, _ := n.api("method").call("GET", "/v1/org", nil); status != http.StatusOK {
		t.Errorf("with the network's secret: status %d, want 200", status)
	}
}

func TestTheAPIRequiresSomeoneActing(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	c := n.api("")
	c.header = false

	if status, _ := c.call("GET", "/v1/org", nil); status != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 when no one is named as acting", status)
	}
}

func TestOnlyPeopleInTheOrgCanUseTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	for _, actor := range []string{"stranger", "kai", "ledger", "GuildServ"} {
		if status, out := n.api(actor).call("GET", "/v1/org", nil); status != http.StatusForbidden {
			t.Errorf("%s: status %d (%v), want 403", actor, status, out)
		}
	}
}

func TestTheAPIWontStartWithoutASecret(t *testing.T) {
	srv, err := ircfake.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	cfg, store, l := testOrg(t, srv.Addr())
	cfg.API.Listen = "127.0.0.1:0"

	r := runner.New(cfg, store, &fakeBrain{}, l, log.New(io.Discard, "", 0))
	err = r.Start()
	t.Cleanup(r.Stop)

	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("Start = %v, want a refusal to serve the API without a secret", err)
	}
}

// The same rules as GuildServ.

func TestTheOrgChartThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	status, out := n.api("method").call("GET", "/v1/org", nil)

	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	names := map[string]map[string]any{}
	for _, p := range out["people"].([]any) {
		m := p.(map[string]any)
		names[m["name"].(string)] = m
	}
	if names["kai"]["kind"] != "agent" || names["kai"]["team"] != "Engineering" || names["kai"]["reports_to"] != "vera" {
		t.Errorf("kai = %v", names["kai"])
	}
	if names["method"]["kind"] != "human" || names["method"]["owner"] != true {
		t.Errorf("the owner = %v", names["method"])
	}
	if len(out["teams"].([]any)) == 0 {
		t.Error("teams should be listed")
	}
}

func TestTheOwnerHiresThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	status, out := n.api("method").call("POST", "/v1/staff", map[string]any{
		"nick": "rex", "role": "QA engineer", "team": "Engineering", "budget": 1.5, "channels": []string{"#engineering"}})

	if status != http.StatusCreated {
		t.Fatalf("status %d: %v", status, out)
	}
	if out["nick"] != "rex" || out["role"] != "QA engineer" || out["reports_to"] == "" {
		t.Errorf("hired = %v", out)
	}
	n.human.say("GuildServ", "INFO rex")
	n.human.expect("rex on staff, as seen from IRC", guildServSaying("method", "QA engineer"))
}

func TestALeadCanOnlyHireIntoTheirOwnTeamThroughTheAPIToo(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)

	status, out := n.api("dana").call("POST", "/v1/staff", map[string]any{"nick": "rex", "team": "Engineering"})
	if status != http.StatusForbidden || !strings.Contains(errorOf(out), "Engineering") {
		t.Errorf("hiring into Engineering: %d %v, want 403 naming the team", status, out)
	}
	status, out = n.api("dana").call("POST", "/v1/staff", map[string]any{"nick": "sal", "role": "SDR"})
	if status != http.StatusCreated || out["team"] != "Sales" || out["reports_to"] != "dana" {
		t.Errorf("hiring into her own team: %d %v, want 201 in Sales reporting to dana", status, out)
	}
}

func TestTeamBudgetsHoldInTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)
	n.human.say("GuildServ", "TEAM SET Sales BUDGET 30")
	// The new budget exactly: "Sales" alone could match an earlier reply
	// (creating the team), and the hire would race ahead of the change.
	n.human.expect("the budget set", guildServSaying("method", "budget $30.00/month"))

	status, out := n.api("dana").call("POST", "/v1/staff", map[string]any{"nick": "sal", "budget": 2})

	if status != http.StatusForbidden || !strings.Contains(errorOf(out), "budget") {
		t.Errorf("status %d %v, want 403 about the team's budget", status, out)
	}
}

func TestBadInputIsRefusedWithTheReason(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	status, out := n.api("method").call("POST", "/v1/staff", map[string]any{"nick": "rex", "model": "gpt-9"})

	if status != http.StatusBadRequest || !strings.Contains(errorOf(out), "gpt-9") {
		t.Errorf("status %d %v, want 400 naming the bad model", status, out)
	}
}

func TestChangingAnAgentThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	status, out := n.api("method").call("PATCH", "/v1/staff/kai", map[string]any{"model": "claude-opus-5", "role": "Staff engineer"})

	if status != http.StatusOK || out["model"] != "claude-opus-5" || out["role"] != "Staff engineer" {
		t.Fatalf("status %d: %v", status, out)
	}
	n.human.say("GuildServ", "INFO kai")
	n.human.expect("the change, as seen from IRC", guildServSaying("method", "claude-opus-5"))
}

func TestALeadCantChangeAnotherTeamsAgentsThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)

	status, _ := n.api("dana").call("PATCH", "/v1/staff/kai", map[string]any{"model": "claude-opus-5"})

	if status != http.StatusForbidden {
		t.Errorf("status %d, want 403", status)
	}
}

func TestFiringThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	status, out := n.api("method").call("DELETE", "/v1/staff/kai", map[string]any{"reason": "budget cuts"})

	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	n.human.expect("kai leaving with the reason", func(m heard) bool {
		return m.quit && m.from == "kai" && strings.Contains(m.text, "Fired by method: budget cuts")
	})
	if status, _ := n.api("method").call("GET", "/v1/staff/kai", nil); status != http.StatusNotFound {
		t.Errorf("kai after firing: status %d, want 404", status)
	}
}

func TestApprovalsThroughTheAPI(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain, withAPI())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	status, out := n.api("method").call("GET", "/v1/approvals", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	list := out["approvals"].([]any)
	if len(list) != 1 || !strings.Contains(list[0].(map[string]any)["summary"].(string), "merge PR #17") {
		t.Fatalf("approvals = %v", list)
	}

	status, out = n.api("method").call("POST", "/v1/approvals/1/deny", map[string]any{"reason": "not before the launch"})

	if status != http.StatusOK || out["status"] != "denied" {
		t.Fatalf("deny: %d %v", status, out)
	}
	n.human.expect("kai hearing why", fromContaining("kai", "#engineering", "not before the launch"))
	if status, out := n.api("method").call("POST", "/v1/approvals/1/approve", nil); status != http.StatusConflict {
		t.Errorf("deciding twice: %d %v, want 409", status, out)
	}
}

func TestSomeoneOutsideTheReportingLineCantDecideThroughTheAPI(t *testing.T) {
	n := startNetwork(t, wantsToMerge(), withAPI())
	withSales(t, n)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	status, out := n.api("dana").call("GET", "/v1/approvals", nil)
	if status != http.StatusOK || len(out["approvals"].([]any)) != 0 {
		t.Errorf("dana's approvals: %d %v, want none", status, out)
	}
	status, _ = n.api("dana").call("POST", "/v1/approvals/1/approve", nil)
	if status != http.StatusForbidden {
		t.Errorf("dana deciding: status %d, want 403", status)
	}
}

func TestAnUnknownPathIsNotFound(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	if status, _ := n.api("method").call("GET", "/v1/nope", nil); status != http.StatusNotFound {
		t.Errorf("status %d, want 404", status)
	}
}

func TestAnAgentsPromptIsOnlyShownToThoseWhoMayChangeIt(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)
	n.api("method").call("PATCH", "/v1/staff/kai", map[string]any{"prompt": "You are kai. The launch codename is BLUEBIRD."})

	_, owner := n.api("method").call("GET", "/v1/staff/kai", nil)
	_, dana := n.api("dana").call("GET", "/v1/staff/kai", nil)

	if owner["prompt"] != "You are kai. The launch codename is BLUEBIRD." {
		t.Errorf("the owner should see kai's prompt: %v", owner)
	}
	if _, shown := dana["prompt"]; shown || dana["nick"] != "kai" {
		t.Errorf("dana leads Sales, not Engineering, so she sees kai but not its prompt: %v", dana)
	}
}

// Teams and members: the owner's tools.

func TestTheOwnerSetsUpATeamThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	owner := n.api("method")

	if status, out := owner.call("POST", "/v1/teams", map[string]any{"name": "Support", "budget": 500, "private": true}); status != http.StatusCreated || out["channel"] != "#support" {
		t.Fatalf("create team: %d %v", status, out)
	}
	if status, out := owner.call("POST", "/v1/members", map[string]any{"account": "sam", "role": "Support lead", "team": "Support", "cost": 4000}); status != http.StatusCreated || out["reports_to"] != "method" {
		t.Fatalf("add member: %d %v", status, out)
	}
	if status, out := owner.call("POST", "/v1/people/sam/promote", nil); status != http.StatusOK || !slices.Contains(strs(out["team"].(map[string]any)["leads"]), "sam") {
		t.Fatalf("promote: %d %v", status, out)
	}
	status, out := owner.call("GET", "/v1/teams/Support/budget", nil)
	if status != http.StatusOK || out["monthly_budget"] != 500.0 || out["committed"] != 4000.0 {
		t.Errorf("budget: %d %v", status, out)
	}

	n.human.say("GuildServ", "TEAMS")
	n.human.expect("the team, as seen from IRC", guildServSaying("method", "Support (#support, private) led by sam, budget $500.00/month"))
}

// A lead manages their own team, and nothing above it.
func TestALeadCantChangeTheOrgAboveTheirTeamThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)
	dana := n.api("dana")

	calls := []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/v1/teams", map[string]any{"name": "Growth"}},
		{"PATCH", "/v1/teams/Sales", map[string]any{"budget": 99999}},
		{"POST", "/v1/members", map[string]any{"account": "eve"}},
		{"PATCH", "/v1/members/dana", map[string]any{"role": "CEO"}},
		{"DELETE", "/v1/members/dana", nil},
		{"POST", "/v1/people/dana/demote", nil},
	}
	for _, c := range calls {
		if status, out := dana.call(c.method, c.path, c.body); status != http.StatusForbidden {
			t.Errorf("dana %s %s: %d %v, want 403", c.method, c.path, status, out)
		}
	}
}

// What people cost is private: only the owner and the team's lead see it.

func TestPeoplesCostsAreOnlyShownToTheOwnerAndTheirLead(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)
	n.human.say("GuildServ", `MEMBER ADD eve ROLE "Account exec" TEAM Sales COST 5000`)
	n.human.expect("eve added", guildServSaying("method", "added eve"))
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))

	_, owner := n.api("method").call("GET", "/v1/teams/Sales/budget", nil)
	_, dana := n.api("dana").call("GET", "/v1/teams/Sales/budget", nil)
	_, sam := n.api("sam").call("GET", "/v1/teams/Sales/budget", nil)

	costOf := func(out map[string]any, name string) (any, bool) {
		for _, c := range out["commitments"].([]any) {
			m := c.(map[string]any)
			if m["name"] == name {
				v, ok := m["monthly"]
				return v, ok
			}
		}
		return nil, false
	}
	if v, ok := costOf(owner, "eve"); !ok || v != 5000.0 {
		t.Errorf("the owner should see eve's cost: %v", owner)
	}
	if v, ok := costOf(dana, "eve"); !ok || v != 5000.0 {
		t.Errorf("Sales' lead should see eve's cost: %v", dana)
	}
	if _, ok := costOf(sam, "eve"); ok {
		t.Errorf("sam isn't in Sales, so shouldn't see eve's cost: %v", sam)
	}
	if _, ok := sam["committed"]; ok {
		t.Errorf("the team's totals would give eve's cost away by subtraction: %v", sam)
	}
	if _, ok := sam["remaining"]; ok {
		t.Errorf("so would what's left: %v", sam)
	}
}

func TestInIRCTooPeoplesCostsAreOnlyForTheOwnerAndTheirLead(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Sales BUDGET 20000")
	n.human.say("GuildServ", `MEMBER ADD eve ROLE "Account exec" TEAM Sales COST 5000`)
	n.human.expect("eve added", guildServSaying("method", "added eve"))
	stranger := joinHuman(t, n.srv.Addr(), "zed")

	stranger.say("GuildServ", "BUDGET Sales")

	m := stranger.expect("the budget", guildServSaying("zed", "Sales"))
	if strings.Contains(m.text, "5000") || strings.Contains(m.text, "eve $") {
		t.Errorf("someone who isn't the owner or Sales' lead saw eve's cost: %q", m.text)
	}
	n.human.say("GuildServ", "BUDGET Sales")
	n.human.expect("the owner seeing it", guildServSaying("method", "eve $5000.00"))
}

func TestAMembersCostIsHiddenFromPeopleOutsideTheirTeam(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)
	n.human.say("GuildServ", `MEMBER ADD eve ROLE "Account exec" TEAM Sales COST 5000`)
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))

	_, bySam := n.api("sam").call("GET", "/v1/members/eve", nil)
	_, byDana := n.api("dana").call("GET", "/v1/members/eve", nil)

	if _, shown := bySam["monthly_cost"]; shown || bySam["role"] != "Account exec" {
		t.Errorf("sam sees eve but not her cost: %v", bySam)
	}
	if byDana["monthly_cost"] != 5000.0 {
		t.Errorf("eve's lead sees her cost: %v", byDana)
	}
}

// Where someone belongs: #general and #announcements, and every channel the
// org chart gives them a rank in (the same rule GuildServ keeps IRC to), so
// the web app can put them there.
func TestYourChannelsThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	withSales(t, n)
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	channels := func(actor string) []string {
		t.Helper()
		status, out := n.api(actor).call("GET", "/v1/me/channels", nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d: %v", actor, status, out)
		}
		var got []string
		for _, c := range out["channels"].([]any) {
			got = append(got, c.(string))
		}
		return got
	}
	check := func(actor string, want, not []string) {
		t.Helper()
		got := channels(actor)
		for _, c := range want {
			if !slices.Contains(got, c) {
				t.Errorf("%s should be in %s: %v", actor, c, got)
			}
		}
		for _, c := range not {
			if slices.Contains(got, c) {
				t.Errorf("%s shouldn't be put in %s: %v", actor, c, got)
			}
		}
	}

	check("method", []string{"#general", "#announcements", "#engineering", "#sales", "#ops"}, nil)
	check("dana", []string{"#general", "#announcements", "#sales", "#ops"}, []string{"#engineering"})
	check("sam", []string{"#general", "#announcements", "#engineering"}, []string{"#sales", "#ops"})
	if status, _ := n.api("kai").call("GET", "/v1/me/channels", nil); status != http.StatusForbidden {
		t.Errorf("an agent asking: %d, want 403 (people only)", status)
	}
}
