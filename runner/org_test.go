// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"
)

// The org chart: teams, human members, and who may change what.

func (n *network) joinAs(account string, channels ...string) *human {
	n.t.Helper()
	return joinHumanLoggedIn(n.t, n.srv.Addr(), account, account, testPassword, channels...)
}

func TestOwnerCreatesATeamWithALead(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "TEAM CREATE Sales LEAD kai")
	n.human.expect("a confirmation", guildServSaying("method", "Sales"))
	n.human.say("GuildServ", "TEAMS")

	n.human.expect("the team list", guildServSaying("method", "Sales (#sales) led by kai"))
}

// Teams used to be called lanes, and people who learned LANE can still use it.
func TestTheOldLaneCommandsStillWork(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "LANE CREATE Sales")
	n.human.expect("a confirmation", guildServSaying("method", "created team Sales"))
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" LANE Sales`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "LANES")

	n.human.expect("the team list", guildServSaying("method", "Sales (#sales)"))
	n.human.say("GuildServ", "INFO dana")
	n.human.expect("dana in Sales", guildServSaying("method", "Sales team"))
}

func TestPrivateTeamsAreMarkedPrivate(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "TEAM CREATE Leadership PRIVATE")
	n.human.expect("a confirmation", guildServSaying("method", "Leadership"))
	n.human.say("GuildServ", "TEAMS")

	n.human.expect("the team list", guildServSaying("method", "Leadership (#leadership, private)"))
}

func TestOwnerAddsAHumanToTheOrg(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.expect("team created", guildServSaying("method", "Sales"))

	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Sales`)
	n.human.expect("a confirmation", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "INFO dana")

	m := n.human.expect("dana's profile", guildServSaying("method", "Head of Sales"))
	for _, want := range []string{"Sales team", "reports to method"} {
		if !strings.Contains(m.text, want) {
			t.Errorf("INFO dana = %q, want it to mention %q", m.text, want)
		}
	}
}

func TestPromoteMakesSomeoneLeadOfTheirTeam(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Sales`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))

	n.human.say("GuildServ", "PROMOTE dana")
	n.human.expect("a confirmation", guildServSaying("method", "lead"))
	n.human.say("GuildServ", "TEAMS")

	n.human.expect("dana leading Sales", guildServSaying("method", "Sales (#sales) led by dana"))
}

// orgWithSalesLedByDana sets up a Sales team with a $500/month budget, led
// by dana, who is logged in.
func orgWithSalesLedByDana(t *testing.T, brain *fakeBrain) (*network, *human) {
	t.Helper()
	n := startNetwork(t, brain)
	n.human.say("GuildServ", "TEAM CREATE Sales BUDGET 500")
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Sales`)
	n.human.say("GuildServ", "PROMOTE dana")
	n.human.expect("dana promoted", guildServSaying("method", "is now lead"))
	return n, n.joinAs("dana", "#general")
}

func TestTeamLeadHiresIntoTheirOwnTeam(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("hello")}
	n, dana := orgWithSalesLedByDana(t, brain)

	dana.say("GuildServ", `HIRE rex ROLE "Account executive" CHANNELS #general`)

	dana.expect("a hire confirmation", guildServSaying("dana", "hired rex"))
	n.human.say("GuildServ", "INFO rex")
	m := n.human.expect("rex's profile", guildServSaying("method", "rex"))
	if !strings.Contains(m.text, "Sales team") || !strings.Contains(m.text, "reports to dana") {
		t.Errorf("INFO rex = %q, want rex in dana's Sales team, reporting to dana", m.text)
	}
}

func TestTeamLeadCantHireIntoAnotherTeam(t *testing.T) {
	_, dana := orgWithSalesLedByDana(t, &fakeBrain{})

	dana.say("GuildServ", `HIRE rex ROLE "Engineer" TEAM Engineering`)

	dana.expect("a refusal", guildServSaying("dana", "can't"))
}

func TestTeamLeadCanOnlyFireAgentsInTheirTeam(t *testing.T) {
	n, dana := orgWithSalesLedByDana(t, &fakeBrain{})
	dana.say("GuildServ", `HIRE rex ROLE "Account executive"`)
	dana.expect("rex hired", guildServSaying("dana", "hired rex"))

	dana.say("GuildServ", "FIRE kai")
	dana.expect("a refusal for kai", guildServSaying("dana", "can't"))
	dana.say("GuildServ", "FIRE rex")
	dana.expect("rex fired", guildServSaying("dana", "fired rex"))

	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
}

func TestMembersWhoArentLeadsCantChangeTheOrg(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Designer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	sam := n.joinAs("sam", "#general")

	for _, cmd := range []string{"TEAM CREATE Design", `HIRE bot ROLE "x" TEAM Engineering`, "PROMOTE sam", "FIRE kai"} {
		sam.say("GuildServ", cmd)
		sam.expect("a refusal for "+cmd, guildServSaying("sam", "can't"))
	}
}

func TestHiringIntoAnUnknownTeamIsRefused(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", `HIRE rex ROLE "x" TEAM Nowhere`)

	n.human.expect("a refusal", guildServSaying("method", "no team"))
}

func TestOrgShowsTheReportingTree(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Designer" TEAM Engineering REPORTS-TO vera`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))

	n.human.say("GuildServ", "ORG")

	var lines []string
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(lines, "\n"), "sam") {
		lines = append(lines, n.human.expect("the org chart", fromGuildServ("method")).text)
	}
	chart := strings.Join(lines, "\n")
	// method at the top, vera under method, kai and sam under vera.
	iMethod, iVera, iKai, iSam := strings.Index(chart, "method"), strings.Index(chart, "vera"), strings.Index(chart, "kai"), strings.Index(chart, "sam")
	if iMethod < 0 || iVera < iMethod || iKai < iVera || iSam < iVera {
		t.Errorf("ORG should list method, then vera, then vera's reports:\n%s", chart)
	}
	if indent(chart, "kai") <= indent(chart, "vera") || indent(chart, "vera") <= indent(chart, "method") {
		t.Errorf("reports should be indented under their manager:\n%s", chart)
	}
}

// indent returns how far the line mentioning name is indented.
func indent(chart, name string) int {
	for _, l := range strings.Split(chart, "\n") {
		if strings.Contains(l, name) {
			return len(l) - len(strings.TrimLeft(l, " "))
		}
	}
	return -1
}

func TestAgentsKnowAboutColleaguesAddedLater(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ok")}
	n := startNetwork(t, brain)
	n.human.say("GuildServ", `HIRE pax ROLE "Marketing lead" TEAM Marketing`)
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Marketing`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))

	n.human.say("#engineering", "vera: who's on the team now?")
	n.human.expect("vera's reply", from("vera", "#engineering"))

	team := brain.lastRequest(t).Team
	for _, want := range []string{"pax", "Marketing lead", "dana", "Head of Sales", "method"} {
		if !strings.Contains(team, want) {
			t.Errorf("vera's view of the team should include %q:\n%s", want, team)
		}
	}
}

// A new lead can't report to someone in the team they now lead (unless
// they lead it too): they move up to the owner.
func TestAPromotedLeadNoLongerReportsToSomeoneInTheirOwnTeam(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.expect("the team", guildServSaying("method", "Sales"))
	n.human.say("GuildServ", `MEMBER ADD eli ROLE "Account exec" TEAM Sales`)
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Sales REPORTS-TO eli`)
	n.human.expect("dana reporting to eli", guildServSaying("method", "reporting to eli"))

	n.human.say("GuildServ", "PROMOTE dana")
	n.human.expect("dana now reporting to the owner", guildServSaying("method", "dana now reports to method"))
	n.human.say("GuildServ", "INFO dana")

	n.human.expect("INFO saying so", guildServSaying("method", "reports to method"))
}

func TestAReportingLoopIsRefused(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" REPORTS-TO kai`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))

	n.human.say("GuildServ", "SET kai REPORTS-TO dana")

	n.human.expect("a refusal", guildServSaying("method", "loop"))
	n.human.say("GuildServ", "INFO kai")
	n.human.expect("kai's line unchanged", func(m heard) bool {
		return guildServSaying("method", "kai")(m) && !strings.Contains(m.text, "reports to dana")
	})
}

func TestAMemberCantBeMadeToReportIntoALoopEither(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" REPORTS-TO kai`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "SET kai REPORTS-TO method")
	n.human.expect("kai updated", guildServSaying("method", "kai"))

	n.human.say("GuildServ", "MEMBER SET dana REPORTS-TO dana")

	n.human.expect("a refusal", guildServSaying("method", "loop"))
}
