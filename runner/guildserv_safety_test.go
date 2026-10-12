// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"
)

// GuildServ is for the people in the company: someone with an account on
// the server who isn't in the org (or isn't logged in) gets no look at it,
// and no catch-up on the network's budget. HELP is still for anyone.
func TestGuildServIsForPeopleInTheOrg(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain)
	n.human.say("#general", "the launch is on friday")
	n.srv.AddAccount("zed", testPassword) // an account on the server, not in the org
	zed := n.joinAs("zed", "#general")
	anon := joinHuman(t, n.srv.Addr(), "anon", "#general")
	time.Sleep(200 * time.Millisecond)

	for nick, h := range map[string]*human{"zed": zed, "anon": anon} {
		for _, cmd := range []string{"STAFF", "TEAMS", "BUDGET", "CATCHUP 7d"} {
			h.say("GuildServ", cmd)
			h.expect(nick+": "+cmd+" refused", guildServSaying(nick, "people in this company"))
		}
		h.say("GuildServ", "HELP")
		h.expect(nick+": help", guildServSaying(nick, "CATCHUP"))
	}
	if c := brain.catchupCount(); c != 0 {
		t.Errorf("%d catch-ups written for people outside the org", c)
	}
}

// HELP is shown whole: no line of it is cut short at IRC's line length.
func TestHelpIsShownWhole(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "HELP")

	for _, end := range []string{"[PRIVATE].", "GOAL DROP <n>.", "ACCESS DROP <level>."} {
		n.human.expect("a line of HELP ending "+end, func(m heard) bool {
			return m.from == "GuildServ" && strings.HasSuffix(m.text, end)
		})
	}
}

// A catch-up is a model call on the network's budget: each person may ask
// for one every ten minutes.
func TestACatchUpIsAtMostOneEveryTenMinutes(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain)
	n.human.say("#general", "the launch is on friday")
	time.Sleep(100 * time.Millisecond)

	n.human.say("GuildServ", "CATCHUP 7d")
	n.human.expect("the catch-up", guildServSaying("method", "summary"))
	n.human.say("GuildServ", "CATCHUP 7d")
	n.human.expect("the second refused", guildServSaying("method", "every 10 minutes"))

	if c := brain.catchupCount(); c != 1 {
		t.Errorf("%d catch-ups written, want 1", c)
	}
}

// Writing a catch-up takes a while; GuildServ answers everyone else
// meanwhile, approvals included.
func TestACatchUpDoesntHoldUpGuildServ(t *testing.T) {
	gate := make(chan struct{})
	brain := &fakeBrain{digest: "summary", catchupGate: gate}
	n := startNetwork(t, brain)
	t.Cleanup(func() { close(gate) })
	n.human.say("#general", "the launch is on friday")
	inOrg(t, n.human, "dana")
	dana := n.joinAs("dana", "#general")
	time.Sleep(100 * time.Millisecond)

	dana.say("GuildServ", "CATCHUP 7d")
	time.Sleep(200 * time.Millisecond)
	n.human.say("GuildServ", "STAFF")

	n.human.expect("STAFF while a catch-up is written", guildServSaying("method", "kai"))
}

// A busy channel's lines that name no one are checked for who should
// answer one at a time: talking fast doesn't run up a model call a line.
func TestUnaddressedLinesAreCheckedOneAtATime(t *testing.T) {
	gate := make(chan struct{})
	brain := &fakeBrain{pickGate: gate}
	n := startNetwork(t, brain, withRelevance())

	for i := range 10 {
		n.human.say("#general", "a thought, number "+strings.Repeat("x", i))
	}
	time.Sleep(500 * time.Millisecond)
	close(gate)
	time.Sleep(300 * time.Millisecond)

	if c := brain.pickCount(); c > 2 {
		t.Errorf("%d checks for 10 lines said at once, want at most 2", c)
	}
}
