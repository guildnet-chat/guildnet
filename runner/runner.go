// SPDX-License-Identifier: AGPL-3.0-only

// Package runner connects every persona to IRC as its own user and decides
// when each one wakes up.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/monitor"
)

const (
	maxReplyLines = 12
	maxLineBytes  = 400 // leaves room for the prefix within IRC's 512-byte line
	lineDelay     = 250 * time.Millisecond
	replyTimeout  = 10 * time.Minute // default for an agent in chat, which may read code through tools first
	deskTimeout   = 30 * time.Minute // default for an agent at a desk, working through tools
	// archiveLines is how much conversation per channel the runner keeps for
	// catch-ups, and how much it reads back from the server on startup.
	archiveLines = 1000
	// awayDelay is how long a reply can take before the agent shows as away,
	// so quick replies don't flicker.
	awayDelay = 500 * time.Millisecond
	// stepEvery throttles tool steps shown as /me lines, so a burst of small
	// steps doesn't flood the channel.
	stepEvery = 3 * time.Second
	// typingEvery renews "typing" while an agent works: clients take it to
	// have stopped after about 6 seconds without one (IRCv3 +typing).
	typingEvery = 3 * time.Second
)

// Brain produces agent replies. *brain.Brain calls each agent's model; tests
// use a fake.
type Brain interface {
	// Respond wakes an agent; onStep hears each tool step as it happens.
	Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error)
	// Resume carries an agent on after an approval decision.
	Resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error)
	PickResponders(ctx context.Context, model, channel, topic string, transcript []chat.Line, candidates []config.Persona) ([]string, ledger.Usage, error)
	Catchup(ctx context.Context, req agent.CatchupRequest) (string, ledger.Usage, error)
}

// Staffing is implemented by agent platforms that keep their own copy of
// each agent (Managed Agents keeps a versioned agent config). The runner
// tells them about hires, changes, and firings.
type Staffing interface {
	Hire(ctx context.Context, p config.Persona) error
	Update(ctx context.Context, p config.Persona) error
	Fire(ctx context.Context, p config.Persona) error
}

// Runner owns one IRC connection per agent, plus the ledger and GuildServ bots.
type Runner struct {
	api     *http.Server // the internal API, if it's on
	apiAddr string
	cfg     *config.Runner
	store   org.Store
	brain   Brain
	ledger  *ledger.Ledger
	chat    *chat.Log
	outside *inboxes      // outside agents' wake-ups, until their programs answer
	toolbox agent.Toolbox // the network's tools for outside agents (GitHub); guarded by mu
	// connectors are the network's connected services (Linear...); guarded by mu.
	connectors Connectors
	// gitRepos are the network's git repositories on any host; guarded by mu.
	gitRepos GitRepos
	// notifier tells the platform news people may want by email; guarded by
	// mu, nil for none.
	notifier Notifier
	// models answers outside agents' model calls (modelcalls.go); guarded
	// by mu, nil for none.
	models http.Handler
	// hooks calls outside agents' webhooks, made once (hookOnce).
	hooks    *http.Client
	hookOnce sync.Once
	logger   *log.Logger

	ledgerConn *ircevent.Connection
	guildConn  *ircevent.Connection

	// staff changes while running, so these are guarded by staffMu.
	staffMu  sync.RWMutex
	personas map[string]config.Persona       // by lowercase nick
	conns    map[string]*ircevent.Connection // by lowercase agent nick
	queues   map[string]chan job             // by lowercase agent nick
	joined   map[string]bool                 // channels the ledger has joined

	sync      syncer     // keeps IRC in step with the org chart
	welcomeMu sync.Mutex // one welcome written at a time
	stop      chan struct{}
	stopOnce  sync.Once

	mu       sync.Mutex
	presence presence             // who's in which channel, and who's away
	wait     waitState            // agents waiting on approvals
	streak   map[string]int       // conversation -> agent messages since a human spoke
	halted   map[string]bool      // conversation -> loop guard tripped
	heard    map[string]time.Time // conversation|nick -> when their last line arrived
	blocked  map[string]string    // agent -> day a budget notice was posted
	// catchupAt is when each account last had a catch-up written (CATCHUP);
	// routing is the channels whose unaddressed lines are being checked.
	catchupAt map[string]time.Time
	routing   map[string]bool
	// resuming are the decisions queued (or held while paused) for their
	// agents to carry on with.
	resuming map[int64]bool
	paused   bool
	onHold   []job    // decisions that came in while paused, for !resume
	twoSteps TwoSteps // how people sign in to the console; nil for no console
}

type job struct {
	persona  config.Persona
	channel  string // set for channel messages
	peer     string // set for DMs
	key      string // conversation key in the chat log
	trigger  chat.Line
	decision *decision // set when this job carries on after an approval
}

// New builds a Runner for the staff in store. Call Start to connect.
func New(cfg *config.Runner, store org.Store, brain Brain, l *ledger.Ledger, logger *log.Logger) *Runner {
	return &Runner{
		cfg: cfg,
		// People, teams and access levels are read over and over to decide
		// who may do what; the runner is the only one changing them.
		store:    org.NewCached(store),
		brain:    brain,
		ledger:   l,
		chat:     chat.NewLog(archiveLines),
		outside:  newInboxes(),
		logger:   logger,
		personas: map[string]config.Persona{},
		conns:    map[string]*ircevent.Connection{},
		queues:   map[string]chan job{},
		joined:   map[string]bool{},
		presence: newPresence(),
		wait:     newWaitState(),
		stop:     make(chan struct{}),
		streak:   map[string]int{},
		halted:   map[string]bool{},
		heard:    map[string]time.Time{},
		blocked:  map[string]string{},
	}
}

// Start connects the ledger bot, GuildServ, and every agent, then returns.
func (r *Runner) Start() error {
	if err := r.checkAPIConfig(); err != nil {
		return err
	}
	r.importTeamRepos()
	// A network someone paused stays paused through a restart.
	paused, err := r.store.Setting(context.Background(), pausedSetting)
	if err != nil {
		return fmt.Errorf("loading whether agents are paused: %w", err)
	}
	r.mu.Lock()
	r.paused = paused != ""
	r.mu.Unlock()
	agents, err := r.store.Agents(context.Background())
	if err != nil {
		return fmt.Errorf("loading staff: %w", err)
	}
	r.staffMu.Lock()
	for _, p := range agents {
		r.personas[strings.ToLower(p.Nick)] = p
	}
	r.staffMu.Unlock()
	// Every account exists before anyone connects, so GuildServ's first sync
	// can rank them.
	for _, nick := range []string{r.cfg.LedgerNick, guildServNick} {
		if err := r.ensureAccount(nick); err != nil {
			return err
		}
	}
	for _, p := range agents {
		if err := r.ensureAccount(p.Nick); err != nil {
			return err
		}
	}

	r.ledgerConn = r.newConn(r.cfg.LedgerNick, "GuildNet ledger (spend, budgets, loop guard)")
	// The ledger reads channel history back from the server after joining,
	// so agents remember recent conversation across restarts.
	// It also asks the server things for GuildServ's commands (labelled, so
	// the answer is told apart), which GuildServ's own connection can't
	// wait for while it handles them.
	r.ledgerConn.RequestCaps = append(r.ledgerConn.RequestCaps, "draft/chathistory", "labeled-response")
	r.ledgerConn.AddCallback("366", func(e ircmsg.Message) { // end of NAMES: we're in
		if len(e.Params) >= 2 {
			// Fill the catch-up archive. Join/quit lines are dropped.
			r.ledgerConn.Send("CHATHISTORY", "LATEST", e.Params[1], "*", strconv.Itoa(archiveLines))
		}
	})
	r.ledgerConn.AddBatchCallback(r.onHistoryBatch)
	r.ledgerConn.AddBatchCallback(onMultiline(r.onLedgerPrivmsg))
	r.trackPresence()
	r.ledgerConn.AddConnectCallback(func(ircmsg.Message) {
		r.staffMu.Lock()
		r.joined = map[string]bool{}
		r.staffMu.Unlock()
		for _, ch := range r.allChannels() {
			r.ledgerJoin(ch)
		}
	})
	r.ledgerConn.AddCallback("PRIVMSG", r.onLedgerPrivmsg)
	r.ledgerConn.AddCallback("TOPIC", func(e ircmsg.Message) {
		if len(e.Params) >= 2 {
			r.chat.SetTopic(e.Params[0], e.Params[1])
		}
	})
	r.ledgerConn.AddCallback("332", func(e ircmsg.Message) { // RPL_TOPIC on join
		if len(e.Params) >= 3 {
			r.chat.SetTopic(e.Params[1], e.Params[2])
		}
	})
	if err := r.ledgerConn.Connect(); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	go r.ledgerConn.Loop()

	r.guildConn = r.newConn(guildServNick, "GuildNet staff services")
	r.guildConn.AddCallback("PRIVMSG", r.onGuildServ)
	r.guildConn.AddBatchCallback(onMultiline(r.onGuildServ))
	r.setupSync()
	if err := r.guildConn.Connect(); err != nil {
		return fmt.Errorf("%s: %w", guildServNick, err)
	}
	go r.guildConn.Loop()

	for _, p := range agents {
		if err := r.startAgent(p); err != nil {
			return err
		}
	}
	r.logger.Printf("connected %d agents to %s", len(agents), r.cfg.Server)
	r.requestSync() // again, now that everyone's here
	go r.watchApprovals(r.stop)
	return r.startAPI()
}

// Stop disconnects everyone.
func (r *Runner) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.stopAPI()
	r.staffMu.Lock()
	conns := make([]*ircevent.Connection, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.staffMu.Unlock()
	for _, c := range conns {
		c.Quit()
	}
	for _, c := range []*ircevent.Connection{r.guildConn, r.ledgerConn} {
		if c != nil {
			c.Quit()
		}
	}
}

// startAgent brings an agent onto the network: its own connection, its
// channels, and a worker for its wake-ups.
func (r *Runner) startAgent(p config.Persona) error {
	key := strings.ToLower(p.Nick)
	if err := r.ensureAccount(p.Nick); err != nil {
		return err
	}
	conn := r.newConn(p.Nick, fmt.Sprintf("%s, %s", p.Name, p.Role))
	conn.AddConnectCallback(func(ircmsg.Message) {
		// Read the current profile: channels may have changed since hiring.
		if cur, ok := r.persona(key); ok {
			for _, ch := range r.channelsFor(cur) {
				conn.Join(ch)
			}
		}
	})
	conn.AddCallback("PRIVMSG", func(e ircmsg.Message) { r.onAgentPrivmsg(key, e) })
	conn.AddBatchCallback(onMultiline(func(e ircmsg.Message) { r.onAgentPrivmsg(key, e) }))

	q := make(chan job, 16)
	r.staffMu.Lock()
	r.personas[key] = p
	r.queues[key] = q
	r.staffMu.Unlock()
	for _, ch := range r.channelsFor(p) {
		r.ledgerJoin(ch)
	}

	if err := conn.Connect(); err != nil {
		r.stopAgent(key, "couldn't connect")
		return fmt.Errorf("%s: %w", p.Nick, err)
	}
	// Only now can others send on it (a connection mid-connect isn't safe to
	// use); until then its connect callback joins its channels itself.
	r.staffMu.Lock()
	r.conns[key] = conn
	r.staffMu.Unlock()
	go conn.Loop()
	go r.worker(q)
	return nil
}

// stopAgent takes an agent off the network. quitMessage is what people see
// when it leaves.
func (r *Runner) stopAgent(nick, quitMessage string) {
	key := strings.ToLower(nick)
	r.staffMu.Lock()
	conn, q := r.conns[key], r.queues[key]
	delete(r.personas, key)
	delete(r.conns, key)
	delete(r.queues, key)
	if q != nil {
		close(q) // under staffMu, so enqueue can't send to it concurrently
	}
	r.staffMu.Unlock()
	if conn != nil {
		if r.accountsOn() {
			// Registered accounts stay online after disconnecting unless told
			// otherwise; an agent that's leaving must really leave.
			conn.Privmsg("NickServ", "SET ALWAYS-ON false")
		}
		conn.QuitMessage = quitMessage
		conn.Quit()
	}
}

// updateAgent applies a changed profile to a running agent. Model, budget,
// and prompt changes take effect on its next wake-up; channel changes now.
func (r *Runner) updateAgent(p config.Persona) {
	key := strings.ToLower(p.Nick)
	r.staffMu.Lock()
	old := r.personas[key]
	r.personas[key] = p
	conn := r.conns[key]
	r.staffMu.Unlock()
	if conn == nil {
		return
	}
	was, now := r.channelsFor(old), r.channelsFor(p)
	for _, ch := range now {
		if !containsChannel(was, ch) {
			r.ledgerJoin(ch)
			conn.Join(ch)
		}
	}
	for _, ch := range was {
		if !containsChannel(now, ch) {
			conn.Send("PART", ch)
		}
	}
}

// rejoinAll has every agent and the ledger (re)join where they belong, for
// channels that turned away an earlier join (e.g. before a rank let them into
// an invite-only channel). Joining a channel you're already in does nothing.
func (r *Runner) rejoinAll() {
	for _, ch := range r.allChannels() {
		r.ledgerConn.Join(ch)
	}
	for _, p := range r.staff() {
		r.staffMu.RLock()
		conn := r.conns[strings.ToLower(p.Nick)]
		r.staffMu.RUnlock()
		if conn == nil {
			continue
		}
		for _, ch := range r.channelsFor(p) {
			conn.Join(ch)
		}
	}
}

// ledgerJoin makes sure the ledger, which makes channel decisions, is in ch.
func (r *Runner) ledgerJoin(ch string) {
	key := strings.ToLower(ch)
	r.staffMu.Lock()
	already := r.joined[key]
	r.joined[key] = true
	r.staffMu.Unlock()
	if !already && r.ledgerConn != nil {
		r.ledgerConn.Join(ch)
	}
}

// persona returns the current profile of an agent on staff.
func (r *Runner) persona(nick string) (config.Persona, bool) {
	r.staffMu.RLock()
	defer r.staffMu.RUnlock()
	p, ok := r.personas[strings.ToLower(nick)]
	return p, ok
}

// staff returns every agent on staff, sorted by nick.
func (r *Runner) staff() []config.Persona {
	r.staffMu.RLock()
	defer r.staffMu.RUnlock()
	out := make([]config.Persona, 0, len(r.personas))
	for _, p := range r.personas {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Nick) < strings.ToLower(out[j].Nick) })
	return out
}

func (r *Runner) newConn(nick, realname string) *ircevent.Connection {
	c := &ircevent.Connection{
		Server:        r.cfg.Server,
		Nick:          nick,
		User:          nick,
		RealName:      realname,
		RequestCaps:   []string{"server-time", "message-tags", "account-tag", "batch", "draft/multiline"},
		QuitMessage:   "Back soon: GuildNet is restarting",
		ReconnectFreq: 5 * time.Second,
	}
	if r.accountsOn() {
		c.UseSASL, c.SASLLogin, c.SASLPassword = true, nick, r.accountPassword(nick)
	}
	// Everyone the runner connects is an agent or a bot: say so (IRCv3 bot
	// mode), so every client can badge them.
	c.AddConnectCallback(func(ircmsg.Message) {
		c.Send("MODE", c.CurrentNick(), "+B")
		if r.accountsOn() {
			r.refreshVhost(nick) // one set while it was signing in may not have reached it
		}
	})
	return c
}

func (r *Runner) allChannels() []string {
	set := map[string]bool{}
	add := func(ch string) {
		if ch != "" {
			set[strings.ToLower(ch)] = true
		}
	}
	for _, ch := range r.cfg.Channels {
		add(ch)
	}
	add(r.cfg.OpsChannel)
	add(r.cfg.GeneralChannel)
	add(r.cfg.AnnouncementsChannel)
	add(r.cfg.FeedbackChannel)
	teams, _ := r.store.Teams(context.Background())
	for _, l := range teams {
		add(l.Channel)
	}
	for _, p := range r.staff() {
		for _, ch := range r.channelsFor(p) {
			add(ch)
		}
	}
	out := make([]string, 0, len(set))
	for ch := range set {
		out = append(out, ch)
	}
	sort.Strings(out)
	return out
}

// onLedgerPrivmsg sees every channel message in order, so all channel
// decisions happen here: the shared log, the loop guard, commands, and which
// agents wake.
func (r *Runner) onLedgerPrivmsg(e ircmsg.Message) {
	if len(e.Params) < 2 {
		return
	}
	target, nick := e.Params[0], e.Nick()
	line, ok := parseLine(e)
	if !ok {
		return
	}
	fromAgent := r.isAgent(nick)
	account := accountOf(e)
	if !isChannel(target) {
		if !fromAgent && strings.HasPrefix(line.Text, "!") {
			r.command(nick, nick, account, line.Text)
		}
		return
	}
	if toRank(target) {
		return // for the channel's ops, say: it isn't the channel's to log or act on
	}
	r.chat.Add(target, line)

	// Agent replies are counted toward the loop guard in handle, once per
	// reply; a human speaking resets the count.
	key := strings.ToLower(target)
	if !fromAgent {
		r.mu.Lock()
		delete(r.streak, key)
		delete(r.halted, key)
		r.mu.Unlock()
	}

	if strings.EqualFold(nick, r.cfg.LedgerNick) || isService(nick) {
		return
	}
	// Agents work for the org: nobody outside it (an account on the server
	// that isn't in the org, or someone taken out of it) wakes them.
	if !fromAgent && !r.inOrgAsPerson(account) {
		return
	}
	if !fromAgent && strings.HasPrefix(line.Text, "!") {
		r.command(target, nick, account, line.Text)
		return
	}
	if r.continues(target, nick, line.Text) {
		return // the rest of a message already on its way to whoever it woke
	}

	// "vera: ... kai ..." is for vera; kai is only being mentioned. With no
	// one addressed first, every agent named in the message wakes.
	var woken []config.Persona
	mayWake := func(p config.Persona) bool {
		return !strings.EqualFold(p.Nick, nick) && r.inChannel(p, target) && (fromAgent || r.mayAsk(account, p))
	}
	if p, ok := r.addressee(line.Text); ok {
		if mayWake(p) {
			woken = append(woken, p)
		}
	} else {
		for _, p := range r.staff() {
			if mentions(line.Text, p.Nick) && mayWake(p) {
				woken = append(woken, p)
			}
		}
	}
	if len(woken) > 0 && fromAgent && r.loopGuard(key, target) {
		return
	}
	for _, p := range woken {
		r.enqueue(job{persona: p, channel: target, key: target, trigger: line})
	}
	if len(woken) == 0 && !fromAgent && r.cfg.Relevance.Enabled && !r.mentionsAnyAgent(line.Text) {
		go r.routeUnaddressed(target, line, account)
	}
}

// onHistoryBatch loads a replayed channel history into the conversation log.
// It never wakes anyone: these messages were already handled when they were
// first said. Other batches are left to the normal handlers.
func (r *Runner) onHistoryBatch(b *ircevent.Batch) bool {
	if b.Command != "BATCH" || len(b.Params) < 3 || b.Params[1] != "chathistory" || !isChannel(b.Params[2]) {
		return false
	}
	var lines []chat.Line
	for _, item := range b.Items {
		if item == nil || item.Command != "PRIVMSG" || len(item.Params) < 2 {
			continue
		}
		// Skip services: Ergo replays joins and quits as HistServ lines.
		if line, ok := parseLine(item.Message); ok && !isService(line.Nick) {
			lines = append(lines, line)
		}
	}
	r.chat.Merge(b.Params[2], lines)
	r.logger.Printf("loaded %d lines of history for %s", len(lines), b.Params[2])
	return true
}

// onAgentPrivmsg wakes an agent when it's DM'd. Channel messages are handled
// by the ledger's connection.
func (r *Runner) onAgentPrivmsg(agentKey string, e ircmsg.Message) {
	if len(e.Params) < 2 || isChannel(e.Params[0]) {
		return
	}
	p, ok := r.persona(agentKey)
	if !ok {
		return // fired
	}
	nick := e.Nick()
	if strings.EqualFold(nick, p.Nick) || strings.EqualFold(nick, r.cfg.LedgerNick) || isService(nick) {
		return
	}
	line, ok := parseLine(e)
	if !ok {
		return
	}
	fromAgent := r.isAgent(nick)
	if !fromAgent && !r.mayAsk(accountOf(e), p) {
		return // not someone the agent works for
	}

	// A DM to this agent.
	key := chat.DMKey(p.Nick, nick)
	r.chat.Add(key, line)
	if r.continues(key, nick, line.Text) {
		return
	}
	if fromAgent {
		pair := []string{strings.ToLower(p.Nick), strings.ToLower(nick)}
		sort.Strings(pair)
		guardKey := "dm:" + pair[0] + "+" + pair[1]
		r.mu.Lock()
		r.streak[guardKey]++
		r.mu.Unlock()
		if r.loopGuard(guardKey, fmt.Sprintf("the DM between %s and %s", pair[0], pair[1])) {
			return
		}
	}
	r.enqueue(job{persona: p, peer: nick, key: key, trigger: line})
}

// loopGuard reports whether agents in a conversation have hit the loop limit.
// It posts a notice the first time the guard trips.
func (r *Runner) loopGuard(key, where string) bool {
	r.mu.Lock()
	over := r.streak[key] >= r.cfg.LoopLimit
	first := over && !r.halted[key]
	if over {
		r.halted[key] = true
	}
	r.mu.Unlock()
	if first {
		msg := fmt.Sprintf("loop guard: agents replied to each other %d times in a row in %s with no human in between, so they'll stop replying to each other there until a human speaks.",
			r.cfg.LoopLimit, where)
		notify := r.cfg.OpsChannel
		if isChannel(where) {
			notify = where
		}
		r.ledgerConn.Privmsg(notify, msg)
		r.logger.Print(msg)
	}
	return over
}

// routeUnaddressed asks the relevance model which agents should answer a
// human message that didn't name anyone: one check per channel at a time,
// so a busy channel isn't a model call a line. Lines said while one runs
// aren't checked on their own.
func (r *Runner) routeUnaddressed(channel string, line chat.Line, account string) {
	defer monitor.Recover()
	if r.isPaused() {
		return
	}
	key := strings.ToLower(channel)
	r.mu.Lock()
	if r.routing[key] {
		r.mu.Unlock()
		return
	}
	if r.routing == nil {
		r.routing = map[string]bool{}
	}
	r.routing[key] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.routing, key)
		r.mu.Unlock()
	}()
	var candidates []config.Persona
	for _, p := range r.staff() {
		if r.inChannel(p, channel) && r.mayAsk(account, p) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return
	}
	if ok, _ := r.ledger.Allowed("", 0); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	recent := r.chat.Recent(channel, 15)
	model := r.smallModel(r.cfg.Relevance.Model)
	picked, u, err := r.brain.PickResponders(ctx, model, channel, r.chat.Topic(channel), recent, candidates)
	r.record("relevance", model, channel, u)
	if err != nil {
		monitor.Problem(r.logger, "relevance filter in %s: %v", channel, err)
		return
	}
	for _, nick := range picked {
		p, ok := r.persona(nick)
		if !ok {
			continue
		}
		r.logger.Printf("relevance: %s picks up %s's message in %s", p.Nick, line.Nick, channel)
		r.enqueue(job{persona: p, channel: channel, key: channel, trigger: line})
	}
}

// enqueue queues j for its agent, and reports whether it did: a full queue
// drops a wake-up (a decision is tried again by replayDecisions).
func (r *Runner) enqueue(j job) bool {
	// Hold staffMu so stopAgent can't close the queue mid-send.
	r.staffMu.RLock()
	defer r.staffMu.RUnlock()
	q, ok := r.queues[strings.ToLower(j.persona.Nick)]
	if !ok {
		return false
	}
	select {
	case q <- j:
		return true
	default:
		r.logger.Printf("%s's queue is full; dropping wake from %s", j.persona.Nick, j.trigger.Nick)
		return false
	}
}

// worker handles one agent's wake-ups one at a time until the agent leaves.
// Wakes that pile up for the same conversation collapse into one reply to the
// latest message.
func (r *Runner) worker(q chan job) {
	defer monitor.Recover()
	for j := range q {
		batch := []job{j}
	drain:
		for {
			select {
			case next := <-q:
				batch = append(batch, next)
			default:
				break drain
			}
		}
		latest := map[string]int{}
		for i, b := range batch {
			latest[strings.ToLower(b.key)] = i
		}
		for i, b := range batch {
			if latest[strings.ToLower(b.key)] == i {
				r.handle(b)
			}
		}
	}
}

func (r *Runner) handle(j job) {
	// Use the current profile: it may have changed since the wake was queued.
	p, ok := r.persona(j.persona.Nick)
	if !ok {
		return
	}
	if j.decision != nil {
		// A decision is kept while agents are paused, and carried on with
		// when they're resumed: the agent mustn't work, but it mustn't be
		// lost either.
		if r.holdWhilePaused(j) {
			return
		}
		if !r.claimResume(j.decision.approval) {
			return // carried on with already
		}
		act := r.working(p.Nick, j)
		defer act.finish()
		r.handleDecision(p, *j.decision, act)
		return
	}
	if r.isPaused() {
		return
	}
	if a, waiting := r.waitingOn(p.Nick); waiting {
		r.park(j, a)
		return
	}
	if ok, why := r.ledger.Allowed(p.Nick, p.DailyBudgetUSD); !ok {
		r.noticeBlocked(p.Nick, why)
		return
	}
	if why := r.outsideCapped(p); why != "" {
		r.noticeBlocked(p.Nick, why)
		return
	}

	trigger := r.settle(j.key, j.trigger)
	req := agent.Request{Persona: p, Trigger: trigger, Team: r.teamSummary(), Goals: r.guidingLights(p.Team), Location: r.loc()}
	if j.channel != "" {
		req.Where = j.channel
		req.Topic = r.chat.Topic(j.channel)
	} else {
		req.Where = "a direct message (DM) with " + j.peer
	}
	req.Transcript = withTrigger(r.chat.Recent(j.key, r.cfg.HistoryLines), j.trigger)

	ctx, cancel := context.WithTimeout(context.Background(), r.timeoutFor(p))
	defer cancel()
	m := r.meter(p, req.Where)
	ctx = agent.WithMeter(ctx, m)
	act := r.working(p.Nick, j)
	defer act.finish()
	if atDesk(p) {
		// Desk work takes minutes, so say so now rather than going quiet.
		act.ack()
	}
	start := time.Now()
	res, err := r.respond(ctx, req, j.channel, j.peer, act.step)
	act.summarize()
	u := res.Usage
	cost := m.settle(res)
	target := j.channel
	if target == "" {
		target = j.peer
	}
	if err != nil {
		// Post what the agent had said before it stopped, then why it stopped.
		said := res.Reply != "" && res.Reply != agent.Pass
		if said {
			act.stopTyping(true)
			r.deliver(p, j.channel, target, j.key, res.Reply, trigger.MsgID)
		}
		monitor.Problem(r.logger, "%s: %v", p.Nick, err)
		r.ledgerConn.Privmsg(r.cfg.OpsChannel, truncate(fmt.Sprintf("%s couldn't finish for %s in %s: %v", p.Nick, j.trigger.Nick, req.Where, err), maxLineBytes))
		if !said {
			// Not silence: whoever asked hears it didn't work (as a reply to
			// them, without their nick, which would wake an agent that asked).
			act.stopTyping(true)
			msg := fmt.Sprintf("I couldn't answer that just now; something went wrong (the details are in %s).", r.cfg.OpsChannel)
			if errors.Is(err, errNotConnected) {
				msg = "I'm not connected right now, so I can't answer: the program that runs me outside GuildNet isn't checking in."
			}
			r.deliver(p, j.channel, target, j.key, msg, trigger.MsgID)
		}
		r.keepWork(p, act, j.channel, trigger.Nick, trigger.Text, outcomeOf(res, err, 0), cost)
		return
	}
	r.logger.Printf("%s woke for %s in %s: %.1fs, $%.4f, in=%d out=%d cache_read=%d",
		p.Nick, j.trigger.Nick, req.Where, time.Since(start).Seconds(), cost, u.Input, u.Output, u.CacheRead)
	if res.Reply != "" && res.Reply != agent.Pass {
		act.stopTyping(true) // the answer itself ends typing
		r.deliver(p, j.channel, target, j.key, res.Reply, trigger.MsgID)
	}
	var waiting int64
	if res.Approval != nil {
		if a, ok := r.raiseApproval(p, j.channel, j.trigger.Nick, res.Approval); ok {
			waiting = a.ID
		}
	}
	r.keepWork(p, act, j.channel, trigger.Nick, trigger.Text, outcomeOf(res, nil, waiting), cost)
}

// deliver posts an agent's reply to a channel, or to target by DM when
// channel is "".
func (r *Runner) deliver(p config.Persona, channel, target, key, reply, replyTo string) {
	r.staffMu.RLock()
	conn := r.conns[strings.ToLower(p.Nick)]
	r.staffMu.RUnlock()
	if conn == nil {
		return // fired while thinking
	}
	if channel != "" {
		// Count the reply before sending it, so the ledger's connection sees
		// the updated count when it decides whether the reply wakes anyone.
		r.mu.Lock()
		r.streak[strings.ToLower(channel)]++
		r.mu.Unlock()
	}
	// The model's words, and what it read, are text: no control codes, and
	// no pinging a crowd.
	reply = r.quietMentions(cleanText(dropSelfAddress(reply, p.Nick)))
	for _, l := range r.send(conn, target, reply, replyTo) {
		if channel == "" { // channel lines come back through the ledger's view
			r.chat.Add(key, chat.Line{At: time.Now(), Nick: p.Nick, Text: l.Text, Action: l.Action})
		}
	}
}

// loc is the company timezone.
func (r *Runner) loc() *time.Location {
	if r.cfg.Location != nil {
		return r.cfg.Location
	}
	return time.UTC
}

// activity shows people what an agent is doing while it works: typing
// where it's working (IRCv3 +typing, renewed every typingEvery), away after
// awayDelay, the latest step in the away message, and in the channel as
// much as its chatter says (see chatter.go): by default that it's working,
// what it achieved, and a summary at the end. Away messages never name who
// DM'd the agent.
type activity struct {
	r        *Runner
	nick     string
	conn     *ircevent.Connection
	target   string // channel, or DM partner
	mu       sync.Mutex
	timer    *time.Timer
	away     bool
	done     bool
	lastStep time.Time
	skipped  int
	// How much of the work shows, and what's been done, for the summary.
	chatter   string
	started   time.Time
	steps     int
	kinds     map[string]int
	order     []string
	announced bool           // said it's working
	log       []org.WorkStep // for the agent's page
	typing    chan struct{}  // closed to stop typing; nil when not typing
}

// working starts showing an agent's activity for job j. Call done when it
// finishes; pass step to the brain.
func (r *Runner) working(nick string, j job) *activity {
	r.staffMu.RLock()
	conn := r.conns[strings.ToLower(nick)]
	r.staffMu.RUnlock()
	a := &activity{r: r, nick: nick, conn: conn, target: j.channel, started: time.Now(), kinds: map[string]int{}}
	if a.target == "" {
		a.target = j.peer
	}
	channel := j.channel
	if j.decision != nil && j.decision.approval.Channel != "" {
		a.target, channel = j.decision.approval.Channel, j.decision.approval.Channel
	} else if j.decision != nil {
		a.target, channel = j.decision.approval.Asker, ""
	}
	a.chatter = r.chatterFor(nick, channel)
	if conn == nil {
		return a
	}
	reason := "working: replying in a DM"
	if j.channel != "" {
		reason = fmt.Sprintf("working: replying to %s in %s", j.trigger.Nick, j.channel)
	}
	a.startTyping()
	a.timer = time.AfterFunc(awayDelay, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.done && !a.away {
			conn.Send("AWAY", reason)
			a.away = true
		}
	})
	return a
}

// startTyping says the agent is typing where it's working, and keeps
// saying so until stopTyping.
func (a *activity) startTyping() {
	if !hasCap(a.conn, "message-tags") || a.target == "" {
		return
	}
	stop := make(chan struct{})
	a.typing = stop
	a.conn.SendWithTags(map[string]string{"+typing": "active"}, "TAGMSG", a.target)
	go func() {
		t := time.NewTicker(typingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				a.conn.SendWithTags(map[string]string{"+typing": "active"}, "TAGMSG", a.target)
			}
		}
	}()
}

// stopTyping stops saying the agent is typing. answered means a message is
// about to go, which ends typing by itself; otherwise say done.
func (a *activity) stopTyping(answered bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.typing == nil {
		return
	}
	close(a.typing)
	a.typing = nil
	if !answered {
		a.conn.SendWithTags(map[string]string{"+typing": "done"}, "TAGMSG", a.target)
	}
}

// ack tells whoever asked that the agent has picked up their request.
func (a *activity) ack() {
	if a.conn == nil {
		return
	}
	a.mu.Lock()
	a.announced = true
	a.mu.Unlock()
	a.conn.Action(a.target, "is on it")
}

// step shows one tool step: in the away message always, and where the
// agent is working as its chatter says. What a step achieved always shows.
func (a *activity) step(s agent.Step) {
	// A step is a line of text, however a tool or what it read put it.
	s.Summary = strings.Join(strings.Fields(cleanText(s.Summary)), " ")
	if a.conn == nil || s.Summary == "" {
		return
	}
	if a.r != nil {
		s.Summary = a.r.quietMentions(s.Summary)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return
	}
	a.conn.Send("AWAY", truncate("working: "+s.Summary, 200))
	a.away = true
	if len(a.log) < maxWorkSteps {
		a.log = append(a.log, org.WorkStep{At: time.Now().UTC(), Summary: truncate(s.Summary, 300), Result: s.Result})
	}
	if !s.Result {
		a.steps++
		k := stepKind(s.Summary)
		if a.kinds[k] == 0 {
			a.order = append(a.order, k)
		}
		a.kinds[k]++
	}
	switch {
	case a.chatter == chatterSteps:
		if !s.Result && !a.lastStep.IsZero() && time.Since(a.lastStep) < stepEvery {
			a.skipped++
			return
		}
		line := s.Summary
		if a.skipped > 0 {
			line += fmt.Sprintf(" (+%d more steps)", a.skipped)
			a.skipped = 0
		}
		a.conn.Action(a.target, truncate(line, maxLineBytes))
		a.lastStep = time.Now()
	case s.Result:
		a.conn.Action(a.target, truncate(s.Summary, maxLineBytes))
	case a.chatter == chatterSummary && !a.announced:
		a.announced = true
		a.conn.Action(a.target, "is working on it")
	}
}

// summarize posts what the work came to, where a summary is wanted and
// there were steps: how long it took, and its steps by kind. Not what it
// cost: not everyone in a channel may see spend.
func (a *activity) summarize() {
	if a.conn == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.chatter != chatterSummary || a.steps == 0 {
		return
	}
	a.conn.Action(a.target, truncate("worked "+took(time.Since(a.started))+": "+describeSteps(a.steps, a.kinds, a.order), maxLineBytes))
}

// finish stops showing activity: back from away, or away as waiting on an
// approval if that's why the work stopped.
func (a *activity) finish() {
	if a.conn == nil {
		return
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	a.stopTyping(false)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = true
	if ap, waiting := a.r.waitingOn(a.nick); waiting {
		a.conn.Send("AWAY", fmt.Sprintf("waiting on approval #%d from %s", ap.ID, ap.Approver))
	} else if a.away {
		a.conn.Send("AWAY")
	}
}

// recordResult books what a wake-up cost: the platform's own charge when it
// reports one (it includes sandbox time), otherwise worked out from tokens.
func (r *Runner) recordResult(who, model, where string, res agent.Result) float64 {
	if res.Cost > 0 {
		cost, crossed := r.ledger.RecordCost(who, res.Cost)
		r.announceBudget(crossed)
		r.ledger.AppendDetail(ledger.Detail{Agent: who, Model: model, Where: where, Usage: res.Usage, Cost: cost})
		return cost
	}
	return r.record(who, model, where, res.Usage)
}

// record books a call in the ledger and announces budget thresholds.
func (r *Runner) record(who, model, where string, u ledger.Usage) float64 {
	if u == (ledger.Usage{}) {
		return 0
	}
	cost, crossed := r.ledger.Record(who, model, u)
	r.announceBudget(crossed)
	r.ledger.AppendDetail(ledger.Detail{Agent: who, Model: model, Where: where, Usage: u, Cost: cost})
	return cost
}

// announceBudget posts the highest budget threshold a call crossed; one call
// can cross several.
func (r *Runner) announceBudget(crossed []int) {
	if len(crossed) == 0 {
		return
	}
	t := crossed[len(crossed)-1]
	msg := fmt.Sprintf("budget: %d%% of today's $%.2f used. %s", t, r.ledger.Budget(), r.ledger.Summary())
	if t >= 100 {
		msg = fmt.Sprintf("budget: today's $%.2f is used up, so agents are stopping until tomorrow (UTC). %s", r.ledger.Budget(), r.ledger.Summary())
		spent, _ := r.ledger.Today()
		r.tellNews(Event{Type: "spend_cap", Spend: &SpendEvent{Spent: spent, Cap: r.ledger.Budget()}})
	}
	r.ledgerConn.Privmsg(r.cfg.OpsChannel, truncate(msg, maxLineBytes))
}

func (r *Runner) noticeBlocked(nick, why string) {
	day := time.Now().UTC().Format("2006-01-02")
	r.mu.Lock()
	already := r.blocked[strings.ToLower(nick)] == day
	r.blocked[strings.ToLower(nick)] = day
	r.mu.Unlock()
	if !already {
		r.ledgerConn.Privmsg(r.cfg.OpsChannel, fmt.Sprintf("%s is off for the rest of the day: %s.", nick, why))
	}
}

// send posts a reply line by line and returns what it sent. If replyTo is
// a message id, the reply says it answers that message (IRCv3 +draft/reply).
func (r *Runner) send(conn *ircevent.Connection, target, reply, replyTo string) []chat.Line {
	first := &firstTags{}
	if replyTo != "" && hasCap(conn, "message-tags") {
		first.tags = map[string]string{"+draft/reply": replyTo}
	}
	if ok, maxBytes, maxLines := multilineLimits(conn); ok {
		return r.sendMultilineReply(conn, target, reply, maxBytes, maxLines, first)
	}
	var sent []chat.Line
	for _, raw := range strings.Split(reply, "\n") {
		if len(sent) >= maxReplyLines {
			break
		}
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		action := strings.HasPrefix(text, "/me ")
		if action {
			text = strings.TrimPrefix(text, "/me ")
		}
		for _, chunk := range wrap(text, maxLineBytes) {
			if err := say(conn, target, chunk, action, first.take()); err != nil {
				monitor.Problem(r.logger, "send to %s: %v", target, err)
				return sent
			}
			sent = append(sent, chat.Line{Text: chunk, Action: action})
			time.Sleep(lineDelay)
		}
	}
	return sent
}

// command runs a !command from a human. reply is a channel or a nick.
func (r *Runner) command(reply, nick, account, text string) {
	cmd := strings.ToLower(strings.Fields(text)[0])
	switch cmd {
	case "!spend":
		if !r.can(account, canSpend, wholeOrg) {
			r.ledgerConn.Privmsg(reply, nick+": you can't see the whole org's spend.")
			return
		}
	case "!pause", "!resume":
		// Every agent, everywhere: for those who run the whole org.
		if !r.can(account, canHire, wholeOrg) && !r.can(account, canApprove, wholeOrg) {
			r.ledgerConn.Privmsg(reply, fmt.Sprintf("%s: you can't %s every agent; the owner and org-wide admins can.", nick, strings.TrimPrefix(cmd, "!")))
			return
		}
		if why := r.notOverIRC(account); why != "" {
			r.ledgerConn.Privmsg(reply, nick+": "+why)
			return
		}
	}
	switch cmd {
	case "!spend":
		r.ledgerConn.Privmsg(reply, r.ledger.Summary())
	case "!pause":
		if err := r.store.SetSetting(context.Background(), pausedSetting, account); err != nil {
			monitor.Problem(r.logger, "pausing: %v", err)
			r.ledgerConn.Privmsg(reply, nick+": that didn't work: the database had a problem. Try again.")
			return
		}
		r.mu.Lock()
		r.paused = true
		r.mu.Unlock()
		r.ledgerConn.Privmsg(reply, fmt.Sprintf("all agents paused by %s. !resume to wake them.", nick))
	case "!resume":
		if err := r.store.SetSetting(context.Background(), pausedSetting, ""); err != nil {
			monitor.Problem(r.logger, "resuming: %v", err)
			r.ledgerConn.Privmsg(reply, nick+": that didn't work: the database had a problem. Try again.")
			return
		}
		r.mu.Lock()
		r.paused = false
		r.halted = map[string]bool{}
		r.streak = map[string]int{}
		held := r.onHold
		r.onHold = nil
		r.mu.Unlock()
		r.ledgerConn.Privmsg(reply, fmt.Sprintf("agents resumed by %s.", nick))
		for _, j := range held {
			r.enqueue(j)
		}
	case "!help":
		r.ledgerConn.Privmsg(reply, "!spend shows today's model spend, !pause stops all agents, !resume starts them again.")
	}
}

// pausedSetting holds who paused every agent (!pause), or "" when they
// aren't: kept in the store, so a restart doesn't wake them.
const pausedSetting = "paused"

func (r *Runner) isPaused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

// holdWhilePaused keeps j until agents are resumed, if they're paused.
func (r *Runner) holdWhilePaused(j job) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		r.onHold = append(r.onHold, j)
	}
	return r.paused
}

func (r *Runner) isAgent(nick string) bool {
	_, ok := r.persona(nick)
	return ok
}

// addressee returns the agent a message is addressed to by the IRC
// convention of starting with "nick:" or "nick,".
func (r *Runner) addressee(text string) (config.Persona, bool) {
	t := strings.TrimPrefix(strings.TrimSpace(text), "@")
	end := strings.IndexAny(t, ":,")
	if end <= 0 {
		return config.Persona{}, false
	}
	return r.persona(t[:end])
}

func (r *Runner) mentionsAnyAgent(text string) bool {
	for _, p := range r.staff() {
		if mentions(text, p.Nick) {
			return true
		}
	}
	return false
}

// channelsFor is where an agent belongs: its own channels plus its team's.
func (r *Runner) channelsFor(p config.Persona) []string {
	chans := append([]string(nil), p.Channels...)
	if p.Team != "" {
		if l, err := r.store.Team(context.Background(), p.Team); err == nil && l.Channel != "" && !containsChannel(chans, l.Channel) {
			chans = append(chans, l.Channel)
		}
	}
	return chans
}

// inChannel reports whether agent p belongs in channel.
func (r *Runner) inChannel(p config.Persona, channel string) bool {
	return containsChannel(r.channelsFor(p), channel)
}

func containsChannel(chans []string, channel string) bool {
	for _, ch := range chans {
		if strings.EqualFold(ch, channel) {
			return true
		}
	}
	return false
}

// isService reports whether nick is an IRC services bot like NickServ.
func isService(nick string) bool {
	return strings.HasSuffix(strings.ToLower(nick), "serv")
}

// accountOf is the account the server says sent e, or "" if none.
func accountOf(e ircmsg.Message) string {
	_, account := e.GetTag("account")
	return account
}

func isChannel(target string) bool {
	return strings.HasPrefix(target, "#") || strings.HasPrefix(target, "&") || toRank(target)
}

// toRank reports whether target is a channel's members of some rank only,
// "@#chan" (Ergo's STATUSMSG: ~&@%+): not a DM, and not the channel either.
func toRank(target string) bool {
	t := strings.TrimLeft(target, "~&@%+")
	return t != target && strings.HasPrefix(t, "#")
}

// parseLine turns a PRIVMSG into a Line. CTCP other than ACTION is ignored.
func parseLine(e ircmsg.Message) (chat.Line, bool) {
	line := chat.Line{At: time.Now(), Nick: e.Nick(), Text: e.Params[1]}
	_, line.MsgID = e.GetTag("msgid")
	if ok, ts := e.GetTag("time"); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			line.At = t
		}
	}
	if strings.HasPrefix(line.Text, "\x01") {
		inner := strings.Trim(line.Text, "\x01")
		if !strings.HasPrefix(inner, "ACTION ") {
			return line, false
		}
		line.Text = strings.TrimPrefix(inner, "ACTION ")
		line.Action = true
	}
	return line, true
}

// mentions reports whether text contains nick as a whole word.
func mentions(text, nick string) bool {
	t, n := strings.ToLower(text), strings.ToLower(nick)
	for i := 0; ; {
		j := strings.Index(t[i:], n)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(n)
		if (start == 0 || !isNickChar(t[start-1])) && (end == len(t) || !isNickChar(t[end])) {
			return true
		}
		i = start + 1
	}
}

func isNickChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '[' || c == ']' || c == '\\' || c == '`' || c == '^' || c == '{' || c == '}' || c == '|'
}

// dropSelfAddress removes a leading "nick:" or "nick," where an agent has
// addressed its own reply to itself.
func dropSelfAddress(reply, nick string) string {
	lines := strings.Split(reply, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if len(t) > len(nick) && strings.EqualFold(t[:len(nick)], nick) && (t[len(nick)] == ':' || t[len(nick)] == ',') {
			lines[i] = strings.TrimSpace(t[len(nick)+1:])
		}
	}
	return strings.Join(lines, "\n")
}

// withTrigger makes sure the waking message is in the transcript. Everything
// posted after it stays too, so the agent doesn't answer a stale view.
func withTrigger(lines []chat.Line, trigger chat.Line) []chat.Line {
	for _, l := range lines {
		if l.Nick == trigger.Nick && l.Text == trigger.Text {
			return lines
		}
	}
	return append(lines, trigger)
}

// wrap splits text into chunks of at most max bytes, on spaces when possible.
func wrap(text string, max int) []string {
	var out []string
	for len(text) > max {
		cut := strings.LastIndexByte(text[:max], ' ')
		if cut <= 0 {
			cut = max
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func truncate(s string, max int) string {
	// One IRC line: a line break would end the message there (and the rest
	// would be read as a command), so lines are joined with spaces.
	if strings.ContainsAny(s, "\r\n") {
		var parts []string
		for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\r' || r == '\n' }) {
			if l = strings.TrimSpace(l); l != "" {
				parts = append(parts, l)
			}
		}
		s = strings.Join(parts, " ")
	}
	if len(s) <= max {
		return s
	}
	cut := max - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// continueWithin is how close together one person's lines must be to count
// as one split-up message; 0 means off.
func (r *Runner) continueWithin() time.Duration {
	return max(r.cfg.ContinueWithin, 0)
}

// continues notes a line's arrival and says whether it carries on the
// sender's previous line in key: their client split a long message, so it's
// part of that message, not a new one. A line addressed to an agent is
// always a new message.
func (r *Runner) continues(key, nick, text string) bool {
	k := strings.ToLower(key + "|" + nick)
	now := time.Now()
	r.mu.Lock()
	last, ok := r.heard[k]
	r.heard[k] = now
	r.mu.Unlock()
	within := r.continueWithin()
	if within == 0 || !ok || now.Sub(last) > within {
		return false
	}
	_, addressed := r.addressee(text)
	return !addressed
}

// settle waits until the person who woke an agent has finished sending
// their message, and returns the whole of it: the trigger and the lines
// that carried straight on from it.
func (r *Runner) settle(key string, trigger chat.Line) chat.Line {
	within := r.continueWithin()
	if within == 0 {
		return trigger
	}
	k := strings.ToLower(key + "|" + trigger.Nick)
	for deadline := time.Now().Add(4 * within); time.Now().Before(deadline); {
		r.mu.Lock()
		last := r.heard[k]
		r.mu.Unlock()
		wait := within - time.Since(last)
		if wait <= 0 {
			break
		}
		time.Sleep(wait)
	}
	lines := r.chat.Recent(key, 200)
	start := -1
	for i, l := range lines {
		if l.Nick == trigger.Nick && l.Text == trigger.Text && l.At.Equal(trigger.At) {
			start = i
		}
	}
	if start < 0 {
		return trigger
	}
	prev := trigger.At
	for _, l := range lines[start+1:] {
		if !strings.EqualFold(l.Nick, trigger.Nick) || l.At.Sub(prev) > within {
			break
		}
		if _, addressed := r.addressee(l.Text); addressed {
			break
		}
		trigger.Text += " " + l.Text
		if l.MsgID != "" {
			trigger.MsgID = l.MsgID // the answer replies to the last of it
		}
		prev = l.At
	}
	return trigger
}

// sendMultilineReply sends a reply as multiline messages: its text lines
// together, and /me actions on their own. Like line-by-line sending, it
// stops after maxReplyLines lines.
func (r *Runner) sendMultilineReply(conn *ircevent.Connection, target, reply string, maxBytes, maxLines int, first *firstTags) []chat.Line {
	var sent []chat.Line
	var texts []string
	flush := func() {
		sent = append(sent, r.sendLines(conn, target, texts, maxBytes, maxLines, first)...)
		texts = nil
	}
	count := 0
	for _, raw := range strings.Split(reply, "\n") {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		if count >= maxReplyLines {
			break
		}
		count++
		if action, ok := strings.CutPrefix(text, "/me "); ok {
			flush()
			for _, chunk := range wrap(action, maxLineBytes) {
				if err := say(conn, target, chunk, true, first.take()); err != nil {
					monitor.Problem(r.logger, "send to %s: %v", target, err)
					return sent
				}
				sent = append(sent, chat.Line{Text: chunk, Action: true})
			}
			continue
		}
		texts = append(texts, text)
	}
	flush()
	return sent
}
