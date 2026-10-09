// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/config"
)

// Keeping IRC in step with the org chart. GuildServ is an IRC operator and
// the founder of every company channel. It registers them with ChanServ,
// sets their modes, and gives people ranks (ChanServ AMODE) by account:
//
//	owner       ~ everywhere
//	team lead   @ in their team, announcements, and ops; % in general;
//	            + in the leadership channel
//	team member + in their team
//	ledger      + in every invite-only channel, so it can follow them
//
// Every company channel is +nt; announcements is +m; ops and private teams
// are +is.

// rankOrder ranks channel modes from highest to lowest.
const rankOrder = "qaohv"

// syncOn reports whether GuildServ can keep IRC in step with the org chart.
func (r *Runner) syncOn() bool {
	return r.accountsOn() && r.cfg.Oper.Name != "" && r.cfg.Oper.Password != ""
}

// syncer holds what's needed to read AMODE listings back from ChanServ.
type syncer struct {
	mu      sync.Mutex // one sync at a time
	listMu  sync.Mutex
	listing *amodeListing
	pending bool // a sync was requested while one was running
	opered  chan struct{}
	vhosts  map[string]string // lowercase name -> hostname HostServ confirmed
	vhostMu sync.Mutex        // guards vhosts, asking, and the retry
	asking  map[string]string // lowercase name -> hostname asked for, awaiting HostServ's reply
	// Refused hostnames are asked for again after backoff, which doubles
	// each time up to vhostRetryMax; retryTimer is the one waiting, if any.
	retryTimer *time.Timer
	backoff    time.Duration
	// feedbackTopic: #feedback's topic has been set since starting.
	feedbackTopic bool
}

type amodeListing struct {
	channel string
	want    int // -1 until the header arrives
	got     map[string]byte
	done    chan struct{}
}

var (
	amodeHeader = regexp.MustCompile(`^Channel (\S+) has (\d+) persistent modes set`)
	amodeLine   = regexp.MustCompile(`^Account (\S+) receives mode \+(\w)`)
)

// setupSync has GuildServ oper up on connect and read ChanServ's replies.
func (r *Runner) setupSync() {
	if !r.syncOn() {
		return
	}
	r.sync.opered = make(chan struct{})
	// Agents may be connecting already (refreshVhost), so under the lock.
	r.sync.vhostMu.Lock()
	r.sync.vhosts = map[string]string{}
	r.sync.asking = map[string]string{}
	r.sync.vhostMu.Unlock()
	var once sync.Once
	gc := r.guildConn
	// HostServ's replies are matched to requests by label.
	gc.RequestCaps = append(gc.RequestCaps, "labeled-response")
	opered := func() {
		once.Do(func() { close(r.sync.opered) })
		r.requestSync()
		// Ergo starts with its own welcome (after a restart, say): make it
		// say what the company is for again.
		r.updateWelcome(true)
	}
	gc.AddConnectCallback(func(ircmsg.Message) {
		gc.Send("OPER", r.cfg.Oper.Name, r.cfg.Oper.Password)
		// GuildServ's account is always-on, so after a restart we may be
		// reattaching to a session that's already an operator, in which
		// case OPER isn't confirmed again. Check our own modes too.
		gc.Send("MODE", guildServNick)
	})
	gc.AddCallback("381", func(ircmsg.Message) { opered() }) // RPL_YOUREOPER
	gc.AddCallback("221", func(e ircmsg.Message) {           // RPL_UMODEIS
		if len(e.Params) >= 2 && strings.Contains(e.Params[1], "o") {
			opered()
		}
	})
	gc.AddCallback("491", func(ircmsg.Message) { // ERR_NOOPERHOST
		r.logger.Print("GuildServ couldn't become an IRC operator; check GUILDNET_OPER_NAME and GUILDNET_OPER_PASSWORD")
	})
	gc.AddCallback("NOTICE", func(e ircmsg.Message) {
		if len(e.Params) >= 2 && strings.EqualFold(e.Nick(), "ChanServ") {
			r.onChanServNotice(e.Params[1])
		}
	})
}

func (r *Runner) onChanServNotice(text string) {
	r.sync.listMu.Lock()
	defer r.sync.listMu.Unlock()
	l := r.sync.listing
	if l == nil {
		return
	}
	if m := amodeHeader.FindStringSubmatch(text); m != nil && strings.EqualFold(m[1], l.channel) {
		l.want = atoiOr(m[2], 0)
	} else if m := amodeLine.FindStringSubmatch(text); m != nil && l.want >= 0 {
		l.got[strings.ToLower(m[1])] = m[2][0]
	} else if strings.Contains(text, "isn't registered") || strings.Contains(text, "not registered") {
		l.want = 0
	}
	if l.want >= 0 && len(l.got) >= l.want {
		close(l.done)
		r.sync.listing = nil
	}
}

// requestSync syncs in the background, coalescing requests that arrive while
// a sync is running.
func (r *Runner) requestSync() {
	if !r.syncOn() {
		return
	}
	r.sync.listMu.Lock()
	if r.sync.pending {
		r.sync.listMu.Unlock()
		return
	}
	r.sync.pending = true
	r.sync.listMu.Unlock()
	go func() {
		r.sync.mu.Lock()
		defer r.sync.mu.Unlock()
		r.sync.listMu.Lock()
		r.sync.pending = false
		r.sync.listMu.Unlock()
		r.syncOrg()
	}()
}

// channelPlan is how one company channel should look.
type channelPlan struct {
	name  string
	modes string          // flags that should be on, e.g. "nt", "ntm", "ntis"
	ranks map[string]byte // lowercase account -> mode letter
}

// managedModes are the channel flags GuildServ controls.
const managedModes = "ntmis"

// plan works out every company channel from the org chart.
func (r *Runner) plan() map[string]*channelPlan {
	ctx := context.Background()
	plans := map[string]*channelPlan{}
	channel := func(name, modes string) *channelPlan {
		// Never a name that isn't one channel's: commands are built from it.
		if name == "" || !validChannel(name) {
			return nil
		}
		k := strings.ToLower(name)
		if p, ok := plans[k]; ok {
			for _, m := range modes {
				if !strings.ContainsRune(p.modes, m) {
					p.modes += string(m)
				}
			}
			return p
		}
		p := &channelPlan{name: name, modes: modes, ranks: map[string]byte{}}
		plans[k] = p
		return p
	}
	grant := func(p *channelPlan, account string, mode byte) {
		if p == nil || account == "" {
			return
		}
		k := strings.ToLower(account)
		if cur, ok := p.ranks[k]; !ok || strings.IndexByte(rankOrder, mode) < strings.IndexByte(rankOrder, cur) {
			p.ranks[k] = mode
		}
	}

	general := channel(r.cfg.GeneralChannel, "nt")
	announcements := channel(r.cfg.AnnouncementsChannel, "ntm")
	channel(r.cfg.FeedbackChannel, "nt")
	ops := channel(r.cfg.OpsChannel, "ntis")
	for _, ch := range r.cfg.Channels {
		channel(ch, "nt")
	}

	teams, _ := r.store.Teams(ctx)
	var leadership *channelPlan
	teamChans := map[string]*channelPlan{}
	for _, l := range teams {
		modes := "nt"
		if l.Private {
			modes = "ntis"
		}
		p := channel(l.Channel, modes)
		teamChans[strings.ToLower(l.Name)] = p
		if strings.EqualFold(l.Name, "Leadership") {
			leadership = p
		}
	}

	// Team members, then leads (a lead's @ outranks their +).
	members, _ := r.store.Members(ctx)
	for _, m := range members {
		grant(teamChans[strings.ToLower(m.Team)], m.Account, 'v')
	}
	for _, a := range r.staff() {
		grant(teamChans[strings.ToLower(a.Team)], a.Nick, 'v')
	}
	for _, l := range teams {
		for _, lead := range l.Leads {
			// A lead runs their team and every team inside it.
			for _, inner := range teams {
				if r.within(inner.Name, []string{l.Name}) {
					grant(teamChans[strings.ToLower(inner.Name)], lead, 'o')
				}
			}
			grant(announcements, lead, 'o')
			grant(ops, lead, 'o')
			grant(general, lead, 'h')
			grant(leadership, lead, 'v')
		}
	}
	for _, p := range plans {
		grant(p, r.cfg.Owner, 'q')
		if strings.Contains(p.modes, "i") {
			grant(p, r.cfg.LedgerNick, 'v')
		}
	}
	return plans
}

// personsChannels is where a person belongs: general and announcements, and
// every channel the plan gives them a rank in (all of them, for the owner).
func (r *Runner) personsChannels(account string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ch string) {
		if ch != "" && !seen[strings.ToLower(ch)] {
			seen[strings.ToLower(ch)] = true
			out = append(out, ch)
		}
	}
	add(r.cfg.GeneralChannel)
	add(r.cfg.AnnouncementsChannel)
	add(r.cfg.FeedbackChannel)
	var ranked []string
	for _, p := range r.plan() {
		if _, ok := p.ranks[strings.ToLower(account)]; ok {
			ranked = append(ranked, p.name)
		}
	}
	sort.Strings(ranked)
	for _, ch := range ranked {
		add(ch)
	}
	return out
}

// syncOrg makes IRC match the plan. It only ever adds and removes the ranks
// and modes it manages, so anything else on a channel is left alone.
func (r *Runner) syncOrg() {
	select {
	case <-r.sync.opered:
	case <-time.After(30 * time.Second):
		r.logger.Print("sync skipped: GuildServ isn't an IRC operator yet")
		return
	}
	gc := r.guildConn
	// Hostnames first: they don't depend on channels, and going through
	// every channel (a ChanServ listing each) can take a while.
	r.syncVhosts()
	plans := r.plan()
	names := make([]string, 0, len(plans))
	for k := range plans {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, k := range names {
		p := plans[k]
		gc.Join(p.name)
		gc.Send("SAMODE", p.name, "+q", guildServNick)
		gc.Privmsg("ChanServ", "REGISTER "+p.name)

		// Ranks before flags: whoever the org chart ranks here (the owner, a
		// lead) can join once the channel is invite-only, but only once
		// they have their rank.
		current := r.listAMODEs(p.name)
		for account, mode := range current {
			if strings.EqualFold(account, guildServNick) {
				continue
			}
			if want, ok := p.ranks[account]; !ok || want != mode {
				gc.Privmsg("ChanServ", "AMODE "+p.name+" -"+string(mode)+" "+account)
			}
		}
		for account, mode := range p.ranks {
			if current[account] != mode {
				gc.Privmsg("ChanServ", "AMODE "+p.name+" +"+string(mode)+" "+account)
				// And now, for anyone already in the channel: the AMODE covers
				// joining later, but someone who joined while the channel was
				// being registered can be missed.
				gc.Send("SAMODE", p.name, "+"+string(mode), account)
			}
		}

		on, off := "+"+p.modes, ""
		for _, m := range managedModes {
			if !strings.ContainsRune(p.modes, m) {
				off += string(m)
			}
		}
		if off != "" {
			on += "-" + off
		}
		gc.Send("MODE", p.name, on)

		// Whoever creates a channel gets op. Our own bots and agents, and
		// anyone else in it, only keep op where the org chart gives it to
		// them; and a private team's channel has only the people the org
		// chart ranks there in it, not whoever was in it before it went
		// private, or before they left the team.
		in := r.whoIsIn(p.name)
		for _, nick := range append(append([]string{r.cfg.LedgerNick}, staffNicks(r.staff())...), in...) {
			if strings.EqualFold(nick, guildServNick) || isService(nick) {
				continue
			}
			if mode, ok := p.ranks[strings.ToLower(nick)]; !ok || strings.IndexByte(rankOrder, mode) > strings.IndexByte(rankOrder, 'o') {
				gc.Send("MODE", p.name, "-o", nick)
			}
		}
		if strings.Contains(p.modes, "i") {
			for _, nick := range in {
				_, ranked := p.ranks[strings.ToLower(nick)]
				if ranked || strings.EqualFold(nick, guildServNick) || strings.EqualFold(nick, r.cfg.LedgerNick) || r.isAgent(nick) || isService(nick) {
					continue
				}
				gc.Send("KICK", p.name, nick, p.name+" is private: only its team's people are in it")
			}
		}
	}
	// #feedback says where what's said there goes: set when it doesn't
	// already (the ledger, in the channel, has heard its topic), and at most
	// once per start. Setting it again announces the same topic to everyone.
	if ch := r.cfg.FeedbackChannel; ch != "" && !r.sync.feedbackTopic {
		if r.chat.Topic(ch) != feedbackTopic {
			gc.Send("TOPIC", ch, feedbackTopic)
		}
		r.sync.feedbackTopic = true
	}
	r.rejoinAll()
	r.logger.Printf("synced %d channels with the org chart", len(plans))
}

// listAMODEs reads a channel's AMODEs back from ChanServ.
func (r *Runner) listAMODEs(channel string) map[string]byte {
	l := &amodeListing{channel: channel, want: -1, got: map[string]byte{}, done: make(chan struct{})}
	r.sync.listMu.Lock()
	r.sync.listing = l
	r.sync.listMu.Unlock()
	r.guildConn.Privmsg("ChanServ", "AMODE "+channel)
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
		r.logger.Printf("sync: no AMODE listing for %s", channel)
		r.sync.listMu.Lock()
		r.sync.listing = nil
		r.sync.listMu.Unlock()
	}
	r.sync.listMu.Lock()
	defer r.sync.listMu.Unlock()
	out := map[string]byte{}
	for k, v := range l.got {
		out[k] = v
	}
	return out
}

func staffNicks(ps []config.Persona) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Nick)
	}
	return out
}

func atoiOr(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
