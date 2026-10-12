// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// GuildServ is the IRC face of the control plane: staff changes happen live,
// without restarting the runner.

func fromGuildServ(to string) func(heard) bool {
	return func(m heard) bool { return m.from == "GuildServ" && strings.EqualFold(m.to, to) }
}

func guildServSaying(to, substr string) func(heard) bool {
	return func(m heard) bool {
		return fromGuildServ(to)(m) && strings.Contains(strings.ToLower(m.text), strings.ToLower(substr))
	}
}

func quitBy(nick string) func(heard) bool {
	return func(m heard) bool { return m.quit && strings.EqualFold(m.from, nick) }
}

func TestOwnerHiresAnAgentWhoJoinsAndAnswers(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string { return "hi, " + req.Persona.Nick + " here" }}
	n := startNetwork(t, brain)

	n.human.say("GuildServ", `HIRE pax ROLE "Marketing lead" TEAM Marketing CHANNELS #general,#engineering`)

	n.human.expect("a hire confirmation", guildServSaying("method", "hired pax"))
	n.waitForMembers("#engineering", "kai", "ledger", "method", "pax", "vera")
	n.human.say("#engineering", "pax: welcome aboard")
	n.human.expect("pax answering", fromContaining("pax", "#engineering", "pax here"))

	req := brain.lastRequest(t)
	if req.Persona.Role != "Marketing lead" || req.Persona.Team != "Marketing" {
		t.Errorf("pax woke as %q in team %q, want the role and team it was hired with", req.Persona.Role, req.Persona.Team)
	}
	if req.Persona.ReportsTo != "method" {
		t.Errorf("pax reports to %q, want the owner by default", req.Persona.ReportsTo)
	}
}

// Someone who isn't logged in to an account can't change anything: a nick
// proves nothing.
func TestSomeoneNotLoggedInCantChangeStaff(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	dana := joinHuman(t, n.srv.Addr(), "dana", "#engineering")

	dana.say("GuildServ", `HIRE pax ROLE "Marketing lead"`)
	dana.expect("a refusal", guildServSaying("dana", "log in"))
	dana.say("GuildServ", "FIRE kai")
	dana.expect("another refusal", guildServSaying("dana", "log in"))

	n.human.expectQuiet("any staff change", 300*time.Millisecond, fromGuildServ("method"))
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
}

func TestStaffChangesNeedTheOwnersAccountNotJustTheirNick(t *testing.T) {
	// The owner is logged in to the method account but using the nick boss.
	n := startNetworkAs(t, &fakeBrain{}, "boss")
	impostor := joinHuman(t, n.srv.Addr(), "method", "#engineering")

	impostor.say("GuildServ", `HIRE pax ROLE "Marketing lead"`)
	impostor.expect("a refusal", guildServSaying("method", "log in"))

	n.human.say("GuildServ", `HIRE pax ROLE "Marketing lead"`)
	n.human.expect("the hire going through", guildServSaying("boss", "hired pax"))
}

func TestHiringATakenNickIsRefused(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", `HIRE vera ROLE "Second Vera"`)

	n.human.expect("a refusal", guildServSaying("method", "already"))
}

func TestFiredAgentLeavesAndStopsAnswering(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("still here")}
	n := startNetwork(t, brain)

	n.human.say("GuildServ", "FIRE kai")

	n.human.expect("a confirmation", guildServSaying("method", "kai"))
	n.waitForMembers("#engineering", "ledger", "method", "vera")
	n.human.say("#engineering", "kai: are you there?")
	n.human.expectQuiet("a reply from kai", 500*time.Millisecond, from("kai", "#engineering"))
}

func TestFiredAgentLeavesSayingWhoFiredItAndWhy(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", `FIRE kai "missed too many standups"`)

	m := n.human.expect("kai quitting", quitBy("kai"))
	if m.text != "Fired by method: missed too many standups" {
		t.Errorf("kai quit with %q, want who fired it and why", m.text)
	}
	n.human.expect("a record in #ops", fromContaining("ledger", "#ops", "method fired kai: missed too many standups"))
}

func TestFiredAgentWithNoReasonSaysWhoFiredIt(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "FIRE kai")

	m := n.human.expect("kai quitting", quitBy("kai"))
	if m.text != "Fired by method" {
		t.Errorf("kai quit with %q, want \"Fired by method\"", m.text)
	}
	n.human.expect("a record in #ops", fromContaining("ledger", "#ops", "method fired kai"))
}

func TestAgentsSayTheyllBeBackWhenTheRunnerStops(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.runner.Stop()

	for _, nick := range []string{"vera", "kai"} {
		m := n.human.expect(nick+" quitting", quitBy(nick))
		if !strings.Contains(m.text, "Back soon") {
			t.Errorf("%s quit with %q, want a back-soon message", nick, m.text)
		}
	}
}

func TestSetChangesAnAgentForItsNextReply(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ok")}
	n := startNetwork(t, brain)

	n.human.say("GuildServ", "SET kai MODEL claude-opus-5")
	n.human.expect("a confirmation", guildServSaying("method", "claude-opus-5"))
	n.human.say("#engineering", "kai: hard question for you")
	n.human.expect("kai's reply", from("kai", "#engineering"))

	if got := brain.lastRequest(t).Persona.Model; got != "claude-opus-5" {
		t.Errorf("kai answered with %q, want the model just set", got)
	}
}

func TestSetChannelsMovesAnAgentButKeepsItInItsTeamChannel(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "SET kai CHANNELS #ops")

	n.human.expect("a confirmation", guildServSaying("method", "kai's channels"))
	n.waitForMembers("#general", "ledger", "method", "vera")
	n.waitForMembers("#ops", "kai", "ledger", "method")
	// Engineering is kai's team, so it stays in #engineering.
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
}

func TestInfoDescribesAnAgent(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "INFO kai")

	m := n.human.expect("kai's profile", guildServSaying("method", "kai"))
	for _, want := range []string{"Backend engineer", "claude-sonnet-5", "vera"} {
		if !strings.Contains(m.text, want) {
			t.Errorf("INFO said %q, want it to mention %q", m.text, want)
		}
	}
}

func TestAnyoneInTheOrgCanListTheStaff(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	inOrg(t, n.human, "dana")
	dana := n.joinAs("dana", "#engineering")

	dana.say("GuildServ", "STAFF")

	m := dana.expect("the staff list", guildServSaying("dana", "vera"))
	if !strings.Contains(m.text, "kai") {
		t.Errorf("STAFF said %q, want every agent listed", m.text)
	}
}
