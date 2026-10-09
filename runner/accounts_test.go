// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/internal/ergotest"
	"github.com/guildnet-chat/guildnet/runner"
)

// Accounts, run against a real Ergo: every agent and bot has its own
// NickServ account and logs in with it, so nobody can take their nicks.

// realNetwork is the runner on a throwaway Ergo, with the owner logged in.
type realNetwork struct {
	t      *testing.T
	ergo   *ergotest.Server
	brain  *fakeBrain
	runner *runner.Runner
	start  func() *runner.Runner
	human  *human
	names  *namesWatcher
	ranks  *ranksWatcher
}

func startRealNetwork(t *testing.T, brain *fakeBrain, opts ...ergotest.Option) *realNetwork {
	t.Helper()
	s, err := ergotest.Start(opts...)
	if errors.Is(err, ergotest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping real-Ergo tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	if err := s.Register(ownerAccount, testPassword); err != nil {
		t.Fatal(err)
	}

	cfg, store, l := testOrg(t, s.Addr)
	cfg.Accounts.APIURL, cfg.Accounts.APIToken, cfg.Accounts.Secret = s.APIURL, s.APIToken, "test-secret"
	cfg.Oper.Name, cfg.Oper.Password = s.OperName, s.OperPassword
	cfg.AnnouncementsChannel = "#announcements"
	cfg.FeedbackChannel = "#feedback"
	cfg.WelcomeFiles = s.Welcome()
	n := &realNetwork{t: t, ergo: s, brain: brain}
	// The runner's log goes with the test's, shown if it fails.
	logs := &testLog{t: t}
	t.Cleanup(logs.close) // last: the runner may log while stopping
	n.start = func() *runner.Runner {
		r := runner.New(cfg, store, brain, l, log.New(logs, "runner: ", log.Lmicroseconds))
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Stop)
		return r
	}
	n.runner = n.start()

	h := joinHumanLoggedIn(t, s.Addr, ownerAccount, ownerAccount, ownerPassword, "#general", "#engineering")
	n.human, n.names, n.ranks = h, watchNames(h), watchRanks(h)
	n.waitFor("#engineering", "kai", "vera")
	return n
}

// testLog writes to the test's log until the test is over (timers may fire
// after), then nowhere.
type testLog struct {
	mu     sync.Mutex
	t      *testing.T
	closed bool
}

func (w *testLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.t.Log(strings.TrimRight(string(b), "\n"))
	}
	return len(b), nil
}

func (w *testLog) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

// restart stops the runner and starts a new one, as a deploy would. On a
// real Ergo the agents' and bots' always-on accounts stay online meanwhile.
func (n *realNetwork) restart() {
	n.runner.Stop()
	time.Sleep(500 * time.Millisecond)
	n.runner = n.start()
}

// namesWatcher keeps the latest NAMES reply per channel.
type namesWatcher struct {
	h       *human
	mu      sync.Mutex
	pending map[string][]string
	names   map[string][]string
}

func watchNames(h *human) *namesWatcher {
	w := &namesWatcher{h: h, pending: map[string][]string{}, names: map[string][]string{}}
	h.conn.AddCallback("353", func(e ircmsg.Message) {
		if len(e.Params) >= 4 {
			w.mu.Lock()
			for _, n := range strings.Fields(e.Params[3]) {
				w.pending[e.Params[2]] = append(w.pending[e.Params[2]], strings.TrimLeft(n, "~&@%+"))
			}
			w.mu.Unlock()
		}
	})
	h.conn.AddCallback("366", func(e ircmsg.Message) {
		if len(e.Params) >= 2 {
			w.mu.Lock()
			w.names[e.Params[1]] = w.pending[e.Params[1]]
			delete(w.pending, e.Params[1])
			w.mu.Unlock()
		}
	})
	return w
}

// waitFor waits until every nick is in ch.
func (n *realNetwork) waitFor(ch string, nicks ...string) {
	n.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n.human.conn.Send("NAMES", ch)
		time.Sleep(300 * time.Millisecond)
		n.names.mu.Lock()
		got := n.names.names[ch]
		n.names.mu.Unlock()
		all := true
		for _, want := range nicks {
			all = all && slices.Contains(got, want)
		}
		if all {
			return
		}
	}
	n.t.Fatalf("%v never all joined %s", nicks, ch)
}

func TestAgentsSpeakFromTheirOwnAccounts(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{reply: replyWith("hi from kai")})

	n.human.say("#engineering", "kai: hello")

	m := n.human.expect("kai's reply", from("kai", "#engineering"))
	if m.account != "kai" {
		t.Errorf("kai spoke from account %q, want its own account kai", m.account)
	}
}

func TestNobodyCanTakeAnAgentsNick(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})

	impostor := joinHuman(t, n.ergo.Addr, "kai")
	time.Sleep(time.Second)

	if got := impostor.conn.CurrentNick(); strings.EqualFold(got, "kai") {
		t.Errorf("an impostor got the nick kai")
	}
}

func TestFiredAgentsLeaveForGoodEvenThoughAccountsStayOnline(t *testing.T) {
	// Registered accounts are always-on by default, so they'd normally stay
	// in their channels after disconnecting. A fired agent must not.
	n := startRealNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", "FIRE kai")
	n.human.expect("kai fired", guildServSaying("method", "fired kai"))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n.human.conn.Send("NAMES", "#engineering")
		time.Sleep(300 * time.Millisecond)
		n.names.mu.Lock()
		gone := !slices.Contains(n.names.names["#engineering"], "kai")
		n.names.mu.Unlock()
		if gone {
			return
		}
	}
	t.Fatal("kai is still in #engineering after being fired")
}

// Someone removed from the org is taken off IRC: their app is disconnected,
// their always-on account leaves its channels, and their password no longer
// signs in. Added again, they can.
func TestSomeoneRemovedFromTheOrgIsTakenOffIRC(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	if err := n.ergo.Register("sam", testPassword); err != nil {
		t.Fatal(err)
	}
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	joinHumanLoggedIn(t, n.ergo.Addr, "sam", "sam", testPassword, "#engineering")
	n.waitFor("#engineering", "sam")

	n.human.say("GuildServ", "MEMBER REMOVE sam")
	n.human.expect("sam removed", guildServSaying("method", "removed sam"))

	deadline := time.Now().Add(10 * time.Second)
	for {
		n.human.conn.Send("NAMES", "#engineering")
		time.Sleep(300 * time.Millisecond)
		n.names.mu.Lock()
		gone := !slices.Contains(n.names.names["#engineering"], "sam")
		n.names.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sam is still in #engineering after being removed")
		}
	}
	if ok, err := n.ergo.SignsIn("sam", testPassword); err != nil || ok {
		t.Errorf("sam's password still signs in after being removed (%v)", err)
	}

	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added again", guildServSaying("method", "added sam"))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if ok, _ := n.ergo.SignsIn("sam", testPassword); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sam's password doesn't sign in once added again")
		}
	}
}

func TestHiredAgentsGetTheirOwnAccount(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{reply: replyWith("rex reporting")})

	n.human.say("GuildServ", `HIRE rex ROLE "Support" TEAM Engineering CHANNELS #general,#engineering`)
	n.human.expect("rex hired", guildServSaying("method", "hired rex"))
	n.waitFor("#engineering", "rex")
	n.human.say("#engineering", "rex: hello")

	m := n.human.expect("rex's reply", from("rex", "#engineering"))
	if m.account != "rex" {
		t.Errorf("rex spoke from account %q, want its own account rex", m.account)
	}
}

// An agent can't be hired under a person's account: they'd log in as the
// agent, and be told what's said to it.
func TestAnAgentIsntHiredUnderSomeonesAccount(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	if err := n.ergo.Register("dee", testPassword); err != nil {
		t.Fatal(err)
	}

	n.human.say("GuildServ", `HIRE dee ROLE "Support" TEAM Engineering`)
	n.human.expect("a refusal", guildServSaying("method", "already someone's IRC account"))

	// An agent fired and hired again keeps its own account.
	n.human.say("GuildServ", "FIRE kai")
	n.human.expect("kai fired", guildServSaying("method", "fired kai"))
	n.human.say("GuildServ", `HIRE kai ROLE "Backend engineer" TEAM Engineering`)
	n.human.expect("kai back", guildServSaying("method", "hired kai"))
}

// Agents and bots are marked as bots (IRCv3 bot mode, +B), so every client
// can badge them: the web app, and apps like Goguma and mIRC that show it.
func TestAgentsAndBotsAreMarkedAsBotsAndPeopleArent(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{reply: replyWith("hi from kai")})
	if err := n.ergo.Register("sam", testPassword); err != nil {
		t.Fatal(err)
	}
	other := joinHumanLoggedIn(t, n.ergo.Addr, "sam", "sam", testPassword, "#engineering")

	n.human.say("#engineering", "kai: hello")
	kai := n.human.expect("kai's reply", from("kai", "#engineering"))
	n.human.say("GuildServ", "HELP")
	guildServ := n.human.expect("GuildServ's help", func(m heard) bool { return strings.EqualFold(m.from, "GuildServ") })
	other.say("#engineering", "morning")
	sam := n.human.expect("sam's message", from("sam", "#engineering"))

	if !kai.bot {
		t.Error("kai's messages should carry the bot tag")
	}
	if !guildServ.bot {
		t.Error("GuildServ's messages should carry the bot tag")
	}
	if sam.bot {
		t.Error("sam is a person, not a bot")
	}
}
