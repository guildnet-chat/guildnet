// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/monitor"
)

const (
	// defaultCatchupWindow is how far back CATCHUP looks when it doesn't know
	// when you were last away.
	defaultCatchupWindow = 24 * time.Hour
	maxCatchupWindow     = 7 * 24 * time.Hour
	// maxCatchupLines caps how much of one channel goes to the model.
	maxCatchupLines = 300
)

// presence is what the ledger knows about who's where, from the IRC server.
type presence struct {
	members   map[string]map[string]bool // channel -> lowercase nicks
	names     map[string]string          // lowercase channel -> display name
	awaySince map[string]time.Time       // lowercase nick -> when they went away
	lastAway  map[string]time.Time       // lowercase nick -> start of their last absence
}

func newPresence() presence {
	return presence{
		members:   map[string]map[string]bool{},
		names:     map[string]string{},
		awaySince: map[string]time.Time{},
		lastAway:  map[string]time.Time{},
	}
}

// trackPresence has the ledger follow channel membership and away status.
func (r *Runner) trackPresence() {
	lc := r.ledgerConn
	lc.RequestCaps = append(lc.RequestCaps, "away-notify")
	lc.AddCallback("353", func(e ircmsg.Message) { // NAMES reply
		if len(e.Params) >= 4 {
			for _, n := range strings.Fields(e.Params[3]) {
				r.memberJoined(e.Params[2], strings.TrimLeft(n, "~&@%+"))
			}
		}
	})
	lc.AddCallback("JOIN", func(e ircmsg.Message) {
		if len(e.Params) >= 1 {
			r.memberJoined(e.Params[0], e.Nick())
		}
	})
	lc.AddCallback("PART", func(e ircmsg.Message) {
		if len(e.Params) >= 1 {
			r.memberLeft(e.Params[0], e.Nick())
		}
	})
	lc.AddCallback("KICK", func(e ircmsg.Message) {
		if len(e.Params) >= 2 {
			r.memberLeft(e.Params[0], e.Params[1])
		}
	})
	lc.AddCallback("QUIT", func(e ircmsg.Message) {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, m := range r.presence.members {
			delete(m, strings.ToLower(e.Nick()))
		}
	})
	lc.AddCallback("NICK", func(e ircmsg.Message) {
		if len(e.Params) < 1 {
			return
		}
		old, nu := strings.ToLower(e.Nick()), strings.ToLower(e.Params[0])
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, m := range r.presence.members {
			if m[old] {
				delete(m, old)
				m[nu] = true
			}
		}
	})
	lc.AddCallback("AWAY", r.onAway)
}

func (r *Runner) memberJoined(channel, nick string) {
	key := strings.ToLower(channel)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.presence.members[key] == nil {
		r.presence.members[key] = map[string]bool{}
	}
	if _, ok := r.presence.names[key]; !ok {
		r.presence.names[key] = channel
	}
	r.presence.members[key][strings.ToLower(nick)] = true
}

func (r *Runner) memberLeft(channel, nick string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.presence.members[strings.ToLower(channel)], strings.ToLower(nick))
}

// channelsOf returns the display names of the channels nick is in, sorted.
func (r *Runner) channelsOf(nick string) []string {
	n := strings.ToLower(nick)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for key, m := range r.presence.members {
		if m[n] {
			out = append(out, r.presence.names[key])
		}
	}
	sort.Strings(out)
	return out
}

// onAway notes when people go away, and sends a catch-up when someone comes
// back after long enough.
func (r *Runner) onAway(e ircmsg.Message) {
	nick := e.Nick()
	if r.isAgent(nick) || strings.EqualFold(nick, r.cfg.LedgerNick) || isService(nick) {
		return
	}
	at := time.Now()
	if ok, ts := e.GetTag("time"); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			at = t
		}
	}
	key := strings.ToLower(nick)
	if len(e.Params) > 0 && e.Params[0] != "" { // going away
		r.mu.Lock()
		if _, already := r.presence.awaySince[key]; !already {
			r.presence.awaySince[key] = at
		}
		r.mu.Unlock()
		return
	}
	r.mu.Lock() // back
	since, wasAway := r.presence.awaySince[key]
	delete(r.presence.awaySince, key)
	if wasAway {
		r.presence.lastAway[key] = since
	}
	r.mu.Unlock()
	if wasAway && r.cfg.Catchup.Enabled && time.Since(since) >= r.cfg.Catchup.After {
		go r.catchup(nick, since, "", true)
	}
}

// guildCatchup runs CATCHUP [#channel] [window].
func (r *Runner) guildCatchup(from string, args []string) {
	if !r.cfg.Catchup.Enabled {
		r.guildReply(from, "catch-ups are turned off on this network.")
		return
	}
	since, only := time.Time{}, ""
	for _, a := range args {
		switch {
		case isChannel(a):
			only = a
		default:
			d, err := parseWindow(a)
			if err != nil {
				r.guildReply(from, "usage: CATCHUP [#channel] [window, like 3h or 2d]")
				return
			}
			since = time.Now().Add(-d)
		}
	}
	if since.IsZero() {
		since = time.Now().Add(-defaultCatchupWindow)
		r.mu.Lock()
		if t, ok := r.presence.lastAway[strings.ToLower(from)]; ok && t.After(since) {
			since = t
		}
		r.mu.Unlock()
	}
	r.catchup(from, since, only, false)
}

// catchup summarizes what nick missed since then in their channels (or just
// only). Automatic catch-ups stay quiet when there's nothing to say.
func (r *Runner) catchup(nick string, since time.Time, only string, auto bool) {
	defer monitor.Recover()
	// Approvals waiting on this person come first: something is stalled on them.
	for _, line := range r.waitingOnYou(nick) {
		r.guildConn.Privmsg(nick, truncate(line, maxLineBytes))
	}
	channels := r.channelsOf(nick)
	if only != "" {
		found := false
		for _, ch := range channels {
			if strings.EqualFold(ch, only) {
				channels, found = []string{ch}, true
				break
			}
		}
		if !found {
			r.guildReply(nick, fmt.Sprintf("you're not in %s.", only))
			return
		}
	}

	var logs []agent.ChannelLog
	for _, ch := range channels {
		var lines []chat.Line
		for _, l := range r.chat.Since(ch, since) {
			if !isService(l.Nick) && !strings.EqualFold(l.Nick, r.cfg.LedgerNick) {
				lines = append(lines, l)
			}
		}
		if len(lines) > maxCatchupLines {
			lines = lines[len(lines)-maxCatchupLines:]
		}
		if len(lines) > 0 {
			logs = append(logs, agent.ChannelLog{Name: ch, Topic: r.chat.Topic(ch), Lines: lines})
		}
	}
	when := since.In(r.loc()).Format("Mon 15:04 MST")
	if len(logs) == 0 {
		if !auto {
			r.guildReply(nick, fmt.Sprintf("nothing new in your channels since %s.", when))
		}
		return
	}
	if ok, why := r.ledger.Allowed("", 0); !ok {
		if !auto {
			r.guildReply(nick, "can't write a catch-up right now: "+why+".")
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	text, u, err := r.brain.Catchup(ctx, agent.CatchupRequest{
		Model: r.smallModel(r.cfg.Catchup.Model), For: nick, Since: since, Location: r.loc(), Channels: logs,
	})
	r.record("catchup", r.smallModel(r.cfg.Catchup.Model), "catchup", u)
	if err != nil || strings.TrimSpace(text) == "" {
		monitor.Problem(r.logger, "catch-up for %s: %v", nick, err)
		if !auto {
			r.guildReply(nick, "couldn't write your catch-up just now. Try again in a minute.")
		}
		return
	}
	r.logger.Printf("catch-up for %s since %s across %d channels", nick, when, len(logs))
	r.guildConn.Privmsg(nick, fmt.Sprintf("Catch-up since %s:", when))
	sent := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || sent >= maxReplyLines {
			continue
		}
		for _, chunk := range wrap(line, maxLineBytes) {
			r.guildConn.Privmsg(nick, chunk)
		}
		sent++
	}
}

// parseWindow reads durations like 90m, 3h, or 2d, up to a week.
func parseWindow(s string) (time.Duration, error) {
	var d time.Duration
	var err error
	if strings.HasSuffix(s, "d") {
		n, convErr := strconv.Atoi(strings.TrimSuffix(s, "d"))
		d, err = time.Duration(n)*24*time.Hour, convErr
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad window %q", s)
	}
	if d > maxCatchupWindow {
		d = maxCatchupWindow
	}
	return d, nil
}
