// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"
)

// IRC mirrors the org chart, against a real Ergo: GuildServ registers the
// company channels, sets their modes, and gives people ranks by account.

// ranksWatcher keeps the latest NAMES reply per channel, with rank prefixes.
type ranksWatcher struct {
	mu      sync.Mutex
	pending map[string][]string
	names   map[string][]string
	errors  []string // numeric error replies, like "473 #leadership"
}

func watchRanks(h *human) *ranksWatcher {
	w := &ranksWatcher{pending: map[string][]string{}, names: map[string][]string{}}
	h.conn.AddCallback("353", func(e ircmsg.Message) {
		if len(e.Params) >= 4 {
			w.mu.Lock()
			w.pending[strings.ToLower(e.Params[2])] = append(w.pending[strings.ToLower(e.Params[2])], strings.Fields(e.Params[3])...)
			w.mu.Unlock()
		}
	})
	h.conn.AddCallback("366", func(e ircmsg.Message) {
		if len(e.Params) >= 2 {
			ch := strings.ToLower(e.Params[1])
			w.mu.Lock()
			w.names[ch] = w.pending[ch]
			delete(w.pending, ch)
			w.mu.Unlock()
		}
	})
	for _, code := range []string{"404", "473", "482"} {
		code := code
		h.conn.AddCallback(code, func(e ircmsg.Message) {
			if len(e.Params) >= 2 {
				w.mu.Lock()
				w.errors = append(w.errors, code+" "+strings.ToLower(e.Params[1]))
				w.mu.Unlock()
			}
		})
	}
	return w
}

// expectRank waits until nick shows in ch with exactly prefix ("" for none).
func (n *realNetwork) expectRank(ch, prefix, nick string) {
	n.t.Helper()
	want := prefix + nick
	var got []string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n.human.conn.Send("NAMES", ch)
		time.Sleep(300 * time.Millisecond)
		n.ranks.mu.Lock()
		got = n.ranks.names[strings.ToLower(ch)]
		n.ranks.mu.Unlock()
		if slices.Contains(got, want) {
			return
		}
	}
	n.t.Fatalf("%s never showed as %q in %s; NAMES: %v", nick, want, ch, got)
}

// expectError waits for a numeric error about ch from the server.
func (w *ranksWatcher) expectError(t *testing.T, code, ch string) {
	t.Helper()
	want := code + " " + strings.ToLower(ch)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		found := slices.Contains(w.errors, want)
		w.mu.Unlock()
		if found {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("never got error %s", want)
}

func TestOwnerIsFounderInEveryCompanyChannel(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})

	for _, ch := range []string{"#general", "#engineering"} {
		n.expectRank(ch, "~", "method")
	}
}

func TestTeamLeadIsOpAndMembersHaveVoiceInTheirTeamChannel(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})

	n.expectRank("#engineering", "@", "vera")
	n.expectRank("#engineering", "+", "kai")
	n.expectRank("#general", "%", "vera")
}

func TestPromotionMovesRanksLive(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.expectRank("#engineering", "@", "vera")

	n.human.say("GuildServ", "PROMOTE kai")
	n.human.expect("kai promoted", guildServSaying("method", "is now lead"))
	n.human.say("GuildServ", "DEMOTE vera")
	n.human.expect("vera demoted", guildServSaying("method", "no longer leads"))

	n.expectRank("#engineering", "@", "kai")
	n.expectRank("#engineering", "+", "vera")
}

func TestRanksStillFollowTheOrgChartAfterARestart(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.expectRank("#engineering", "@", "vera")

	n.restart()
	n.human.say("GuildServ", "PROMOTE kai")
	n.human.expect("kai promoted", guildServSaying("method", "is now lead"))

	n.expectRank("#engineering", "@", "kai")
}

func TestOnlyOpsCanChangeTheTopic(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.expectRank("#engineering", "@", "vera")
	outsider := joinHuman(t, n.ergo.Addr, "outsider", "#engineering")
	w := watchRanks(outsider)
	time.Sleep(time.Second)

	outsider.setTopic("#engineering", "hijacked")

	w.expectError(t, "482", "#engineering")
}

func TestPrivateTeamChannelsKeepOutsidersOut(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Leadership PRIVATE")
	n.human.expect("team created", guildServSaying("method", "created team Leadership"))
	n.human.conn.Join("#leadership")
	n.expectRank("#leadership", "~", "method")

	outsider := joinHuman(t, n.ergo.Addr, "outsider")
	w := watchRanks(outsider)
	time.Sleep(time.Second)
	outsider.conn.Join("#leadership")

	w.expectError(t, "473", "#leadership")
}

// Someone who was in a channel before it became a private team's, or before
// they left the team, doesn't stay: only the team's people are in it, and
// nobody keeps an op the org chart doesn't give them.
func TestAPrivateTeamsChannelHasOnlyItsPeopleInIt(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	if err := n.ergo.Register("sam", testPassword); err != nil {
		t.Fatal(err)
	}
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	joinHumanLoggedIn(t, n.ergo.Addr, "sam", "sam", testPassword, "#leadership")
	n.human.conn.Join("#leadership")
	n.expectRank("#leadership", "@", "sam") // whoever makes a channel gets op

	n.human.say("GuildServ", "TEAM CREATE Leadership PRIVATE")
	n.human.expect("team created", guildServSaying("method", "created team Leadership"))

	deadline := time.Now().Add(15 * time.Second)
	for {
		n.human.conn.Send("NAMES", "#leadership")
		time.Sleep(300 * time.Millisecond)
		n.ranks.mu.Lock()
		got := n.ranks.names["#leadership"]
		n.ranks.mu.Unlock()
		if !slices.ContainsFunc(got, func(s string) bool { return strings.TrimLeft(s, "~&@%+") == "sam" }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sam is still in the private #leadership: %v", got)
		}
	}
}

// A team can't take over a channel that's already there and isn't the
// org's: people made it for something else, maybe private. The owner can.
func TestATeamCantTakeAChannelThatsSomeoneElses(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	if err := n.ergo.Register("dana", testPassword); err != nil {
		t.Fatal(err)
	}
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineering manager" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "TEAM SET Engineering LEADS vera,dana")
	n.human.expect("dana leads", guildServSaying("method", "Engineering"))
	dana := joinHumanLoggedIn(t, n.ergo.Addr, "dana", "dana", testPassword)
	hr := joinHuman(t, n.ergo.Addr, "harriet", "#hr-private")
	hr.conn.Send("MODE", "#hr-private", "+i")
	time.Sleep(time.Second)

	dana.say("GuildServ", "TEAM CREATE Ops2 PARENT Engineering CHANNEL #hr-private")
	dana.expect("refused", guildServSaying("dana", "already a channel"))
	n.human.say("GuildServ", "TEAM CREATE Ops3 PARENT Engineering CHANNEL #hr-private")
	n.human.expect("the owner may", guildServSaying("method", "created team Ops3"))
}

// A private team's channel keeps outsiders out, not its own members: the
// people and agents in the team can join it.
func TestPrivateTeamsMembersCanJoinTheirChannel(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Leadership PRIVATE")
	n.human.expect("team created", guildServSaying("method", "created team Leadership"))
	n.human.conn.Join("#leadership")
	n.expectRank("#leadership", "~", "method")
	if err := n.ergo.Register("sam", testPassword); err != nil {
		t.Fatal(err)
	}
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Chief of staff" TEAM Leadership`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	n.human.say("GuildServ", "SET kai TEAM Leadership")
	n.human.expect("kai moved", guildServSaying("method", "kai"))

	sam := joinHumanLoggedIn(t, n.ergo.Addr, "sam", "sam", testPassword)
	w := watchRanks(sam)
	n.expectRank("#leadership", "+", "kai")
	time.Sleep(time.Second) // sam's rank, given alongside kai's, is in place too
	sam.conn.Join("#leadership")

	n.expectRank("#leadership", "+", "sam")
	w.mu.Lock()
	defer w.mu.Unlock()
	if slices.Contains(w.errors, "473 #leadership") {
		t.Error("sam was refused (invite only) from their own team's channel")
	}
}

func TestOnlyLeadsCanSpeakInAnnouncements(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	n.human.conn.Join("#announcements")
	n.expectRank("#announcements", "~", "method")
	outsider := joinHuman(t, n.ergo.Addr, "outsider", "#announcements")
	w := watchRanks(outsider)
	time.Sleep(time.Second)

	outsider.say("#announcements", "hello everyone")

	w.expectError(t, "404", "#announcements")
}
