// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/ircfake"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/runner"
)

const waitTimeout = 5 * time.Second

// network is a running fake IRC server with the runner and one human on it.
type network struct {
	t        *testing.T
	srv      *ircfake.Server
	runner   *runner.Runner
	brain    *fakeBrain
	human    *human
	ledger   *ledger.Ledger
	cfg      *config.Runner
	store    org.Store
	platform runner.Brain // what the runner runs on: brain, or a router
}

// restartRunner stops the runner and starts a fresh one with the same staff
// and config, the way a deploy or crash would.
func (n *network) restartRunner() {
	n.t.Helper()
	n.runner.Stop()
	for _, nick := range []string{"kai", "ledger", "vera"} {
		n.waitForAbsent("#engineering", nick)
	}
	r := runner.New(n.cfg, n.store, n.platform, n.ledger, log.New(io.Discard, "", 0))
	if err := r.Start(); err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(r.Stop)
	n.runner = r
}

// waitForAbsent waits until nick has left ch.
func (n *network) waitForAbsent(ch, nick string) {
	n.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if !slices.Contains(n.srv.Members(ch), nick) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	n.t.Fatalf("%s never left %s", nick, ch)
}

type option func(*config.Runner, *[]config.Persona)

func withRelevance() option {
	return func(c *config.Runner, _ *[]config.Persona) {
		c.Relevance.Enabled = true
		c.Relevance.Model = "claude-haiku-4-5"
	}
}

func withLoopLimit(n int) option {
	return func(c *config.Runner, _ *[]config.Persona) { c.LoopLimit = n }
}

func withBudget(usd float64) option {
	return func(c *config.Runner, _ *[]config.Persona) { c.DailyBudgetUSD = usd }
}

// The owner's account, which the network config names as owner. Every test
// account uses the same password.
const (
	ownerAccount  = "method"
	testPassword  = "hunter2"
	ownerPassword = testPassword
)

// startNetwork brings up vera and kai in #general and #engineering, the
// ledger bot, and the owner, logged in and on nick method, in every channel.
func startNetwork(t *testing.T, brain *fakeBrain, opts ...option) *network {
	t.Helper()
	return startNetworkAs(t, brain, ownerAccount, opts...)
}

// startNetworkAs is startNetwork with the owner connected under ownerNick.
func startNetworkAs(t *testing.T, brain *fakeBrain, ownerNick string, opts ...option) *network {
	t.Helper()
	return startNetworkWithBrain(t, brain, brain, ownerNick, opts...)
}

// startNetworkWith runs the network on any brain, such as a platform router.
func startNetworkWith(t *testing.T, b runner.Brain, opts ...option) *network {
	t.Helper()
	return startNetworkWithBrain(t, nil, b, ownerAccount, opts...)
}

func startNetworkWithBrain(t *testing.T, brain *fakeBrain, b runner.Brain, ownerNick string, opts ...option) *network {
	t.Helper()
	srv, err := ircfake.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	for _, account := range []string{ownerAccount, "dana", "sam"} {
		srv.AddAccount(account, testPassword)
	}

	cfg, store, l := testOrg(t, srv.Addr(), opts...)
	r := runner.New(cfg, store, b, l, log.New(io.Discard, "", 0))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)

	h := joinHumanLoggedIn(t, srv.Addr(), ownerNick, ownerAccount, ownerPassword, "#general", "#engineering", "#ops")
	n := &network{t: t, srv: srv, runner: r, brain: brain, human: h, ledger: l, cfg: cfg, store: store, platform: b}
	n.waitForMembers("#engineering", sortedNicks("kai", "ledger", ownerNick, "vera")...)
	n.waitForMembers("#ops", sortedNicks("ledger", ownerNick)...)
	return n
}

// testOrg is the config, org chart, and ledger every test network starts
// with: vera (lead) and kai in Engineering, an empty Marketing team, and the
// owner method.
func testOrg(t *testing.T, server string, opts ...option) (*config.Runner, *org.Memory, *ledger.Ledger) {
	t.Helper()
	cfg, seed, l := testOrgSeed(t, server, opts...)
	return cfg, org.NewMemory(seed), l
}

// testOrgSeed is testOrg's starting org chart, for tests that bring their
// own store.
func testOrgSeed(t *testing.T, server string, opts ...option) (*config.Runner, org.Seed, *ledger.Ledger) {
	t.Helper()
	cfg := &config.Runner{
		Server:         server,
		Owner:          ownerAccount,
		Channels:       []string{"#general", "#engineering"},
		OpsChannel:     "#ops",
		LedgerNick:     "ledger",
		DailyBudgetUSD: 5,
		LoopLimit:      6,
		HistoryLines:   40,
		DataDir:        t.TempDir(),
		// The same defaults config.Load fills in.
		GeneralChannel:       "#general",
		AnnouncementsChannel: "#announcements",
		Location:             time.UTC,
		// Off by default so wake-ups aren't held back; split-message tests
		// turn it on.
		ContinueWithin: -1,
	}
	cfg.Catchup.Enabled = true
	cfg.Catchup.Model = "claude-haiku-4-5"
	cfg.Catchup.After = time.Hour
	personas := []config.Persona{
		{Nick: "vera", Name: "Vera", Role: "Team lead", Team: "Engineering", ReportsTo: "method", Model: "claude-opus-5",
			Channels: []string{"#general", "#engineering"}},
		{Nick: "kai", Name: "Kai", Role: "Backend engineer", Team: "Engineering", ReportsTo: "vera", Model: "claude-sonnet-5",
			Channels: []string{"#general", "#engineering"}},
	}
	for _, o := range opts {
		o(cfg, &personas)
	}
	seed := org.Seed{
		Agents: personas,
		Teams: []org.Team{
			{Name: "Engineering", Channel: "#engineering", Leads: []string{"vera"}},
			{Name: "Marketing", Channel: "#marketing"},
		},
		Members: []org.Member{{Account: ownerAccount, Role: "Owner"}},
	}
	l, err := ledger.Open(cfg.DataDir, cfg.DailyBudgetUSD)
	if err != nil {
		t.Fatal(err)
	}
	// Before its directory goes: work still finishing writes nothing there.
	t.Cleanup(l.Close)
	return cfg, seed, l
}

func sortedNicks(nicks ...string) []string {
	slices.Sort(nicks)
	return nicks
}

func (n *network) waitForMembers(ch string, want ...string) {
	n.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		got := n.srv.Members(ch)
		if slices.Equal(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	n.t.Fatalf("%s members = %v, want %v", ch, n.srv.Members(ch), want)
}

// fakeBrain stands in for Claude. Tests script its replies.
type fakeBrain struct {
	mu       sync.Mutex
	reply    func(req agent.Request) string
	usage    ledger.Usage
	pick     []string
	digest   string
	requests []agent.Request
	catchups []agent.CatchupRequest

	// approval, when set, can pause a wake-up for approval; afterResume is
	// the reply once a decision comes in.
	approval    func(req agent.Request) *agent.ApprovalRequest
	afterResume func(allow bool, reason string) string
	resumes     []resumeCall

	// steps are tool steps the agent takes before replying; cost is what
	// the platform charges per wake-up; staffing records hire/update/fire.
	steps    func(req agent.Request) []string // a step starting "=> " is a result
	cost     float64
	staffing []string
	fail     error // the wake-up ends in this error, after its reply

	// calls are model calls a wake-up makes, one after another, each asking
	// the wake-up's meter first and charged there as it returns.
	calls []ledger.Usage
}

// metered makes b's calls under ctx's meter, adding them to res; it stops
// with the meter's reason once the budget's used up.
func (b *fakeBrain) metered(ctx context.Context, model string, res agent.Result) (agent.Result, error) {
	b.mu.Lock()
	calls := b.calls
	b.mu.Unlock()
	m := agent.MeterOf(ctx)
	for _, u := range calls {
		if m != nil {
			if err := m.Allow(); err != nil {
				return res, err
			}
			m.Charge(model, u, 0)
		}
		res.Usage.Input += u.Input
		res.Usage.Output += u.Output
	}
	return res, nil
}

func (b *fakeBrain) Hire(_ context.Context, p config.Persona) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.staffing = append(b.staffing, "hire "+p.Nick)
	return nil
}

func (b *fakeBrain) Update(_ context.Context, p config.Persona) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.staffing = append(b.staffing, "update "+p.Nick+" "+p.Model)
	return nil
}

func (b *fakeBrain) Fire(_ context.Context, p config.Persona) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.staffing = append(b.staffing, "fire "+p.Nick)
	return nil
}

type resumeCall struct {
	agent, token, reason string
	allow                bool
}

func (b *fakeBrain) Resume(_ context.Context, p config.Persona, token string, allow bool, reason string, _ func(agent.Step)) (agent.Result, error) {
	b.mu.Lock()
	b.resumes = append(b.resumes, resumeCall{agent: p.Nick, token: token, allow: allow, reason: reason})
	after := b.afterResume
	b.mu.Unlock()
	if after == nil {
		return agent.Result{}, nil
	}
	return agent.Result{Reply: after(allow, reason)}, nil
}

func (b *fakeBrain) lastResume(t *testing.T) resumeCall {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.resumes) == 0 {
		t.Fatal("no agent was resumed")
	}
	return b.resumes[len(b.resumes)-1]
}

func (b *fakeBrain) Catchup(_ context.Context, req agent.CatchupRequest) (string, ledger.Usage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.catchups = append(b.catchups, req)
	return b.digest, ledger.Usage{Input: 500, Output: 100}, nil
}

func (b *fakeBrain) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	reply, usage, approval, steps, cost, fail := b.reply, b.usage, b.approval, b.steps, b.cost, b.fail
	b.mu.Unlock()
	if res, err := b.metered(ctx, req.Persona.Model, agent.Result{}); err != nil {
		return res, err
	}
	if steps != nil {
		for _, s := range steps(req) {
			result, isResult := strings.CutPrefix(s, "=> ")
			onStep(agent.Step{Summary: result, Result: isResult})
		}
	}
	if fail != nil {
		text := ""
		if reply != nil {
			text = reply(req)
		}
		return agent.Result{Reply: text, Usage: usage, Cost: cost}, fail
	}
	if approval != nil {
		if a := approval(req); a != nil {
			return agent.Result{Usage: usage, Cost: cost, Approval: a}, nil
		}
	}
	if reply == nil {
		return agent.Result{Reply: agent.Pass, Usage: usage, Cost: cost}, nil
	}
	return agent.Result{Reply: reply(req), Usage: usage, Cost: cost}, nil
}

func (b *fakeBrain) PickResponders(context.Context, string, string, string, []chat.Line, []config.Persona) ([]string, ledger.Usage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pick, ledger.Usage{Input: 100, Output: 5}, nil
}

func (b *fakeBrain) lastRequest(t *testing.T) agent.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		t.Fatal("no agent was woken")
	}
	return b.requests[len(b.requests)-1]
}

func replyWith(text string) func(agent.Request) string {
	return func(agent.Request) string { return text }
}

// heard is a message the human received.
type heard struct {
	from, to, text string
	account        string // the sender's account, as the server vouches for it
	bot            bool   // the server marks the sender as a bot (IRCv3 bot mode)
	msgid          string // the message's id, as the server gave it
	replyTo        string // the msgid this message answers (+draft/reply)
	typing         string // for a typing notice (TAGMSG +typing): active, paused, or done
	action         bool
	quit           bool // someone left the network; text is their quit message
	away           bool // an away change; text is the reason, "" when back
}

// human is a person connected with an ordinary IRC client.
type human struct {
	t       *testing.T
	conn    *ircevent.Connection
	mu      sync.Mutex
	heard   []heard
	matched map[int]bool // messages already returned by expect
}

// joinHuman connects someone who isn't logged in to an account.
func joinHuman(t *testing.T, addr, nick string, channels ...string) *human {
	t.Helper()
	return joinHumanLoggedIn(t, addr, nick, "", "", channels...)
}

// joinHumanLoggedIn connects someone logged in to account with SASL, or not
// logged in at all when account is "".
func joinHumanLoggedIn(t *testing.T, addr, nick, account, password string, channels ...string) *human {
	t.Helper()
	return joinHumanWith(t, addr, nick, account, password, false, channels...)
}

// joinMultilineHuman connects someone whose client supports multiline: it
// sends long messages as one, and shows a multiline message as one.
func joinMultilineHuman(t *testing.T, addr, nick string, channels ...string) *human {
	t.Helper()
	return joinHumanWith(t, addr, nick, nick, testPassword, true, channels...)
}

func joinHumanWith(t *testing.T, addr, nick, account, password string, multiline bool, channels ...string) *human {
	t.Helper()
	h := &human{t: t, matched: map[int]bool{}}
	h.conn = &ircevent.Connection{Server: addr, Nick: nick, User: nick, RealName: nick,
		RequestCaps: []string{"account-tag", "message-tags", "server-time", "away-notify"}}
	if multiline {
		h.conn.RequestCaps = append(h.conn.RequestCaps, "batch", "draft/multiline")
		h.conn.AddBatchCallback(func(b *ircevent.Batch) bool {
			if b.Command != "BATCH" || len(b.Params) < 3 || b.Params[1] != "draft/multiline" || len(b.Items) == 0 {
				return false
			}
			first := b.Items[0]
			m := heard{from: first.Nick(), to: b.Params[2]}
			if ok, account := first.GetTag("account"); ok {
				m.account = account
			}
			m.bot, _ = first.GetTag("bot")
			_, m.msgid = b.GetTag("msgid") // a multiline message's id and reply are on its BATCH line
			_, m.replyTo = b.GetTag("+draft/reply")
			for i, item := range b.Items {
				if len(item.Params) < 2 {
					continue
				}
				if joined, _ := item.GetTag("draft/multiline-concat"); i > 0 && !joined {
					m.text += "\n"
				}
				m.text += item.Params[1]
			}
			h.mu.Lock()
			h.heard = append(h.heard, m)
			h.mu.Unlock()
			return true
		})
	}
	if account != "" {
		h.conn.UseSASL, h.conn.SASLLogin, h.conn.SASLPassword = true, account, password
	}
	// Ready once signed on and in every channel, so nothing the test says or
	// expects races the joins.
	ready := make(chan struct{})
	var readyOnce sync.Once
	var joinMu sync.Mutex
	waiting := map[string]bool{}
	for _, ch := range channels {
		waiting[strings.ToLower(ch)] = true
	}
	h.conn.AddConnectCallback(func(ircmsg.Message) {
		if len(channels) == 0 {
			readyOnce.Do(func() { close(ready) })
		}
		for _, ch := range channels {
			h.conn.Join(ch)
		}
	})
	h.conn.AddCallback("JOIN", func(e ircmsg.Message) {
		if len(e.Params) < 1 || !strings.EqualFold(e.Nick(), h.conn.CurrentNick()) {
			return
		}
		joinMu.Lock()
		delete(waiting, strings.ToLower(e.Params[0]))
		done := len(waiting) == 0
		joinMu.Unlock()
		if done {
			readyOnce.Do(func() { close(ready) })
		}
	})
	record := func(e ircmsg.Message) {
		if len(e.Params) < 2 || !strings.Contains(e.Source, "!") {
			return // skip server notices
		}
		m := heard{from: e.Nick(), to: e.Params[0], text: e.Params[1]}
		if ok, account := e.GetTag("account"); ok {
			m.account = account
		}
		m.bot, _ = e.GetTag("bot")
		_, m.msgid = e.GetTag("msgid")
		_, m.replyTo = e.GetTag("+draft/reply")
		if strings.HasPrefix(m.text, "\x01ACTION ") {
			m.text = strings.TrimSuffix(strings.TrimPrefix(m.text, "\x01ACTION "), "\x01")
			m.action = true
		}
		h.mu.Lock()
		h.heard = append(h.heard, m)
		h.mu.Unlock()
	}
	h.conn.AddCallback("PRIVMSG", record)
	h.conn.AddCallback("NOTICE", record) // services like GuildServ reply by notice
	h.conn.AddCallback("TAGMSG", func(e ircmsg.Message) {
		ok, state := e.GetTag("+typing")
		if !ok || len(e.Params) < 1 {
			return
		}
		h.mu.Lock()
		h.heard = append(h.heard, heard{from: e.Nick(), to: e.Params[0], typing: state})
		h.mu.Unlock()
	})
	h.conn.AddCallback("AWAY", func(e ircmsg.Message) {
		m := heard{from: e.Nick(), away: true}
		if len(e.Params) > 0 {
			m.text = e.Params[0]
		}
		h.mu.Lock()
		h.heard = append(h.heard, m)
		h.mu.Unlock()
	})
	h.conn.AddCallback("QUIT", func(e ircmsg.Message) {
		m := heard{from: e.Nick(), quit: true}
		if len(e.Params) > 0 {
			m.text = e.Params[0]
		}
		h.mu.Lock()
		h.heard = append(h.heard, m)
		h.mu.Unlock()
	})
	if err := h.conn.Connect(); err != nil {
		t.Fatal(err)
	}
	go h.conn.Loop()
	t.Cleanup(h.conn.Quit)
	select {
	case <-ready:
	case <-time.After(waitTimeout):
		t.Fatalf("%s never signed on and joined %v", nick, channels)
	}
	return h
}

// part is a line of a multiline message; concat parts join the line before
// them without a line break (a long line the client wrapped).
type part struct {
	text   string
	concat bool
}

func line(text string) part   { return part{text: text} }
func concat(text string) part { return part{text: text, concat: true} }

var batchRefs atomic.Int64

// sayMultiline sends one message made of several lines, as a multiline
// client does.
func (h *human) sayMultiline(target string, parts ...part) {
	h.t.Helper()
	ref := fmt.Sprintf("ml%d", batchRefs.Add(1))
	must := func(err error) {
		if err != nil {
			h.t.Fatal(err)
		}
	}
	must(h.conn.SendRaw("BATCH +" + ref + " draft/multiline " + target))
	for _, p := range parts {
		tags := map[string]string{"batch": ref}
		if p.concat {
			tags["draft/multiline-concat"] = ""
		}
		must(h.conn.SendWithTags(tags, "PRIVMSG", target, p.text))
	}
	must(h.conn.SendRaw("BATCH -" + ref))
}

func (h *human) say(target, text string) {
	h.t.Helper()
	if err := h.conn.Privmsg(target, text); err != nil {
		h.t.Fatal(err)
	}
}

// away marks the human away, as Ergo does when their last client disconnects.
func (h *human) away(reason string) {
	h.t.Helper()
	if err := h.conn.Send("AWAY", reason); err != nil {
		h.t.Fatal(err)
	}
}

// back clears away, as Ergo does when they reconnect.
func (h *human) back() {
	h.t.Helper()
	if err := h.conn.Send("AWAY"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *human) setTopic(ch, topic string) {
	h.t.Helper()
	if err := h.conn.Send("TOPIC", ch, topic); err != nil {
		h.t.Fatal(err)
	}
}

// expect waits for a message matching ok that no earlier expect returned,
// in whatever order messages arrive, and returns the earliest one.
func (h *human) expect(desc string, ok func(heard) bool) heard {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for i, m := range h.heard {
			if !h.matched[i] && ok(m) {
				h.matched[i] = true
				h.mu.Unlock()
				return m
			}
		}
		h.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("never heard %s; heard %+v", desc, h.all())
	return heard{}
}

// expectQuiet waits d, then fails if any message not already returned by
// expect matches bad.
func (h *human) expectQuiet(desc string, d time.Duration, bad func(heard) bool) {
	h.t.Helper()
	time.Sleep(d)
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, m := range h.heard {
		if !h.matched[i] && bad(m) {
			h.t.Fatalf("expected no %s, but heard %+v", desc, m)
		}
	}
}

func (h *human) all() []heard {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]heard(nil), h.heard...)
}

func from(nick, to string) func(heard) bool {
	return func(m heard) bool {
		return m.typing == "" && strings.EqualFold(m.from, nick) && strings.EqualFold(m.to, to)
	}
}

// typingFrom matches a typing notice from nick in to.
func typingFrom(nick, to, state string) func(heard) bool {
	return func(m heard) bool {
		return m.typing == state && strings.EqualFold(m.from, nick) && strings.EqualFold(m.to, to)
	}
}

func fromContaining(nick, to, substr string) func(heard) bool {
	return func(m heard) bool {
		return from(nick, to)(m) && strings.Contains(strings.ToLower(m.text), strings.ToLower(substr))
	}
}

func fromAgent(to string) func(heard) bool {
	return func(m heard) bool {
		return m.typing == "" && (m.from == "vera" || m.from == "kai") && strings.EqualFold(m.to, to)
	}
}
