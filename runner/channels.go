// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/internal/org"
)

// Channel names people choose: a team's, and an agent's channels.
// GuildServ builds ChanServ commands from them, so each must be one
// channel's name and nothing more.

// channelName is # and up to 50 letters, digits, - _ or .: nothing a
// command built from it could be split at (a space, a comma).
var channelName = regexp.MustCompile(`^#[\p{L}\p{N}_.-]{1,50}$`)

func validChannel(ch string) bool { return channelName.MatchString(ch) }

// companyChannels are the channels every network has, which no team may
// take as its own: a team's leads get op in its channel.
func (r *Runner) companyChannels() []string {
	return []string{r.cfg.GeneralChannel, r.cfg.AnnouncementsChannel, r.cfg.FeedbackChannel, r.cfg.OpsChannel}
}

// checkTeamChannel says what's wrong with ch as team's channel, or "".
func (r *Runner) checkTeamChannel(team, ch string) string {
	if !validChannel(ch) {
		return fmt.Sprintf("%q isn't a channel: # and up to 50 letters, digits, - _ or .", ch)
	}
	for _, c := range r.companyChannels() {
		if c != "" && strings.EqualFold(c, ch) {
			return fmt.Sprintf("%s is already the company's own channel: give the team its own with CHANNEL #name.", ch)
		}
	}
	teams, err := r.store.Teams(context.Background())
	if err != nil {
		return "couldn't check the other teams' channels just now. Try again."
	}
	for _, t := range teams {
		if !strings.EqualFold(t.Name, team) && strings.EqualFold(t.Channel, ch) {
			return fmt.Sprintf("%s is already %s's channel: give the team its own with CHANNEL #name.", ch, t.Name)
		}
	}
	return ""
}

// mayTakeChannel refuses ch as a team's channel for actor if it's already
// there and isn't the org's: people made it for something else, maybe
// private, and the team's ranks and modes would replace its own. The owner
// may. Only where GuildServ runs the channels (sync) can it be taken over.
func (r *Runner) mayTakeChannel(actor, ch string) error {
	if !r.syncOn() || r.isOwner(actor) {
		return nil
	}
	if _, ours := r.plan()[strings.ToLower(ch)]; ours {
		return nil
	}
	// Asked as the ledger: GuildServ's own commands come in on GuildServ's
	// connection, which can't take the answer while one waits for it.
	lines := replyLines(r.askServer(r.ledgerConn, "MODE", ch))
	if len(lines) == 0 {
		return refuse(failed, "couldn't check whether %s is already a channel just now. Try again.", ch)
	}
	for _, m := range lines {
		if m.Command == "403" { // ERR_NOSUCHCHANNEL
			return nil
		}
	}
	return refuse(conflict, "%s is already a channel on the network, made by someone for something else: give the team another, or ask the owner, who may take it over.", ch)
}

// whoIsIn is who's in ch now, by nick, as the server tells GuildServ.
func (r *Runner) whoIsIn(ch string) []string {
	var out []string
	for _, m := range replyLines(r.askServer(r.guildConn, "NAMES", ch)) {
		if m.Command == "353" && len(m.Params) >= 4 {
			for _, n := range strings.Fields(m.Params[3]) {
				out = append(out, strings.TrimLeft(n, "~&@%+"))
			}
		}
	}
	return out
}

// serverWait is how long GuildServ waits for the server to answer it.
const serverWait = 5 * time.Second

// askServer sends command on conn (one that asks for labeled-response) and
// waits for the server's labelled reply: nil if none came soon enough.
// Never call it from conn's own callbacks.
func (r *Runner) askServer(conn *ircevent.Connection, command string, params ...string) *ircevent.Batch {
	got := make(chan *ircevent.Batch, 1)
	err := conn.SendWithLabel(func(b *ircevent.Batch) {
		select {
		case got <- b:
		default:
		}
	}, nil, command, params...)
	if err != nil {
		return nil
	}
	select {
	case b := <-got:
		return b
	case <-time.After(serverWait):
		return nil
	}
}

// replyLines are the messages of a labelled reply.
func replyLines(b *ircevent.Batch) []ircmsg.Message {
	if b == nil {
		return nil
	}
	if len(b.Items) == 0 {
		return []ircmsg.Message{b.Message}
	}
	var out []ircmsg.Message
	for _, item := range b.Items {
		out = append(out, replyLines(item)...)
	}
	return out
}

// mayPutAgentIn refuses to put an agent in a private channel for someone
// who isn't in it themselves: the agent reads it, and can be asked what's
// there.
func (r *Runner) mayPutAgentIn(actor string, chans []string) error {
	if r.isOwner(actor) {
		return nil
	}
	plans := r.plan()
	for _, ch := range chans {
		p := plans[strings.ToLower(ch)]
		if p == nil || !strings.Contains(p.modes, "i") {
			continue
		}
		if _, in := p.ranks[strings.ToLower(actor)]; !in {
			return refuse(forbidden, "%s is private: only someone in it can put an agent there.", ch)
		}
	}
	return nil
}

// mayShareFromPrivateTeams refuses to give a team (before, as after) repos
// or connected services a private team uses, for someone who isn't in that
// team's channel: the team's agents would read them, and can be asked
// what's there. Other teams' are shared freely.
func (r *Runner) mayShareFromPrivateTeams(actor string, before, after org.Team) error {
	if r.isOwner(actor) {
		return nil
	}
	repos, conns := addedFold(before.Repos, after.Repos), addedFold(before.Connectors, after.Connectors)
	if len(repos) == 0 && len(conns) == 0 {
		return nil
	}
	teams, err := r.store.Teams(context.Background())
	if err != nil {
		return r.storeRefusal(after.Name, err)
	}
	plans := r.plan()
	for _, t := range teams {
		if !t.Private || strings.EqualFold(t.Name, after.Name) {
			continue
		}
		if p := plans[strings.ToLower(t.Channel)]; p != nil {
			if _, in := p.ranks[strings.ToLower(actor)]; in {
				continue
			}
		}
		for _, repo := range repos {
			if slices.ContainsFunc(t.Repos, func(x string) bool { return strings.EqualFold(x, repo) }) {
				return refuse(forbidden, "%s is private team %s's: only someone in %s can share it.", repo, t.Name, t.Name)
			}
		}
		for _, c := range conns {
			if slices.ContainsFunc(t.Connectors, func(x string) bool { return strings.EqualFold(x, c) }) {
				return refuse(forbidden, "%s is connected for private team %s: only someone in %s can share it.", c, t.Name, t.Name)
			}
		}
	}
	return nil
}

// addedFold is what's in after and not in before, ignoring letter case.
func addedFold(before, after []string) []string {
	var out []string
	for _, x := range after {
		if !slices.ContainsFunc(before, func(y string) bool { return strings.EqualFold(x, y) }) {
			out = append(out, x)
		}
	}
	return out
}

// added is what's in after and not in before.
func added(before, after []string) []string {
	var out []string
	for _, ch := range after {
		if !containsChannel(before, ch) {
			out = append(out, ch)
		}
	}
	return out
}
