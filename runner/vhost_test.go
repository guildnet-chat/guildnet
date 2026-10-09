// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"
)

// Role hostnames, against a real Ergo: everyone's hostname shows their place
// in the org chart, so /whois and joins tell you who's who.

// hostOf asks the server for nick's hostname until it equals want.
func (n *realNetwork) expectHost(nick, want string) {
	n.t.Helper()
	var mu sync.Mutex
	got := ""
	id := n.human.conn.AddCallback("311", func(e ircmsg.Message) { // RPL_WHOISUSER
		if len(e.Params) >= 4 && strings.EqualFold(e.Params[1], nick) {
			mu.Lock()
			got = e.Params[3]
			mu.Unlock()
		}
	})
	defer n.human.conn.RemoveCallback(id)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n.human.conn.Send("WHOIS", nick)
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		done := got == want
		mu.Unlock()
		if done {
			return
		}
	}
	mu.Lock()
	defer mu.Unlock()
	n.t.Fatalf("%s's hostname is %q, want %q", nick, got, want)
}

func TestHostnamesShowRoleTeamAndWhetherItsAnAI(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})

	n.expectHost("method", "owner.guildnet")
	n.expectHost("vera", "ai-lead.engineering.guildnet")
	n.expectHost("kai", "ai.engineering.guildnet")
	n.expectHost("ledger", "bot.guildnet")
}

func TestHostnamesFollowTheOrgChart(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.expectHost("kai", "ai.engineering.guildnet")

	n.human.say("GuildServ", "PROMOTE kai")
	n.human.expect("kai promoted", guildServSaying("method", "is now lead"))
	n.human.say("GuildServ", "DEMOTE vera")
	n.human.expect("vera demoted", guildServSaying("method", "no longer leads"))

	n.expectHost("kai", "ai-lead.engineering.guildnet")
	n.expectHost("vera", "ai.engineering.guildnet")
}

// Someone added to the org before they have an IRC account can't be given a
// hostname yet. Once they register, the next change to the org gives them
// theirs; it isn't marked as done when the server refused it.
func TestSomeoneWhoRegistersLaterStillGetsTheirHostname(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineer" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	time.Sleep(time.Second) // the sync tries dana's hostname, and the server refuses it

	if err := n.ergo.Register("dana", testPassword); err != nil {
		t.Fatal(err)
	}
	joinHumanLoggedIn(t, n.ergo.Addr, "dana", "dana", testPassword, "#general")
	n.human.say("GuildServ", `MEMBER ADD eve ROLE "Engineer" TEAM Engineering`)
	n.human.expect("eve added", guildServSaying("method", "added eve"))

	n.expectHost("dana", "engineering.guildnet")
}

// A refused hostname is asked for again by itself, not only when the org
// next changes.
func TestARefusedHostnameIsAskedForAgain(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineer" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	time.Sleep(time.Second) // the sync tries dana's hostname, and the server refuses it

	if err := n.ergo.Register("dana", testPassword); err != nil {
		t.Fatal(err)
	}
	joinHumanLoggedIn(t, n.ergo.Addr, "dana", "dana", testPassword, "#general")
	n.expectHost("dana", "engineering.guildnet")
}
