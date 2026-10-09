// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/ergochat/irc-go/ircevent"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Role hostnames: GuildServ sets everyone's hostname (an IRC "vhost") from
// the org chart, so /whois, joins, and nick lists show who's who:
//
//	ada@owner.guildnet           the owner
//	dana@lead.sales.guildnet        a human team lead
//	sam@sales.guildnet              a human in a team
//	kai@ai-lead.engineering.guildnet an agent team lead
//	rex@ai.engineering.guildnet     an agent in a team
//	pat@guest.guildnet              a guest
//	ledger@bot.guildnet             GuildNet's own bots

func (r *Runner) vhostDomain() string {
	if r.cfg.VhostDomain != "" {
		return r.cfg.VhostDomain
	}
	return "guildnet"
}

// vhostFor is the hostname a person should have.
func (r *Runner) vhostFor(p person) string {
	domain := r.vhostDomain()
	switch {
	case r.isOwner(p.name):
		return "owner." + domain
	case p.guest:
		return "guest." + domain
	}
	var parts []string
	lead := slices.ContainsFunc(r.teamsLedBy(p.name), func(t string) bool { return strings.EqualFold(t, p.team) })
	switch {
	case p.agent && lead:
		parts = append(parts, "ai-lead")
	case p.agent:
		parts = append(parts, "ai")
	case lead:
		parts = append(parts, "lead")
	}
	if slug := r.teamSlug(p.team); slug != "" {
		parts = append(parts, slug)
	} else if len(parts) == 0 {
		parts = append(parts, "team")
	}
	return strings.Join(append(parts, domain), ".")
}

// teamSlug is the team's channel name without the #, e.g. "customer-success".
func (r *Runner) teamSlug(team string) string {
	if team == "" {
		return ""
	}
	if l, err := r.store.Team(context.Background(), team); err == nil && l.Channel != "" {
		return strings.ToLower(strings.TrimLeft(l.Channel, "#&"))
	}
	return strings.ToLower(strings.Join(strings.Fields(team), "-"))
}

// How soon (then at most how rarely) refused hostnames are asked for again,
// and how long a reply may take before it counts as lost. (The IRC library
// only gives up on a reply after its own timeout, a minute, and nothing is
// asked for again while a request is outstanding.)
const (
	vhostRetryFirst = 2 * time.Second
	vhostRetryMax   = 5 * time.Minute
	vhostReplyWait  = 5 * time.Second
)

// syncVhosts gives everyone in the org their role hostname and takes it away
// from anyone who's left. A hostname counts as set only once HostServ says
// so: the server refuses one for an account that doesn't exist yet (an agent
// being set up, or someone who hasn't registered), and it's asked for again
// soon, then less often. HostServ's replies don't name the account, so each
// request is labelled (IRCv3 labeled-response) and its reply comes back
// with the same label; one that never comes back counts as refused.
// Unchanged hostnames, and ones already asked for, send nothing.
func (r *Runner) syncVhosts() {
	want := map[string]string{}
	for _, p := range r.people() {
		want[p.name] = r.vhostFor(p)
	}
	for _, bot := range []string{r.cfg.LedgerNick, guildServNick} {
		want[bot] = "bot." + r.vhostDomain()
	}
	r.sync.vhostMu.Lock()
	defer r.sync.vhostMu.Unlock()
	seen := map[string]bool{}
	for name, v := range want {
		k := strings.ToLower(name)
		seen[k] = true
		if asking, ok := r.sync.asking[k]; r.sync.vhosts[k] != v && (!ok || asking != v) {
			r.askHostServ(k, v, "SET "+name+" "+v)
		}
	}
	for k := range r.sync.vhosts {
		if asking, ok := r.sync.asking[k]; !seen[k] && (!ok || asking != "") {
			r.askHostServ(k, "", "DEL "+k)
		}
	}
}

// askHostServ asks HostServ to give k the hostname v ("" takes it away).
// Called with vhostMu held.
func (r *Runner) askHostServ(k, v, command string) {
	r.sync.asking[k] = v
	err := r.guildConn.SendWithLabel(func(reply *ircevent.Batch) { r.onHostServReply(k, v, reply) }, nil, "PRIVMSG", "HostServ", command)
	if err != nil {
		monitor.Problem(r.logger, "asking HostServ for %s's hostname: %v", k, err)
		delete(r.sync.asking, k)
		r.retryVhosts()
		return
	}
	// A reply that doesn't come soon counts as lost, so the hostname is
	// asked for again; one that comes later is ignored, and asking again
	// does no harm.
	time.AfterFunc(vhostReplyWait, func() { r.onHostServReply(k, v, nil) })
}

// onHostServReply takes HostServ's answer to asking for k's hostname: nil
// if none came.
func (r *Runner) onHostServReply(k, v string, reply *ircevent.Batch) {
	r.sync.vhostMu.Lock()
	defer r.sync.vhostMu.Unlock()
	if asking, ok := r.sync.asking[k]; !ok || asking != v {
		return // asked for something else since; that has its own answer coming
	}
	delete(r.sync.asking, k)
	if !hostServSucceeded(reply) {
		r.logger.Printf("HostServ didn't give %s the hostname %q: %s", k, v, hostServSaid(reply))
		r.retryVhosts() // refused, or no answer: asked for again soon
		return
	}
	r.sync.backoff = 0
	if v == "" {
		delete(r.sync.vhosts, k)
	} else {
		r.sync.vhosts[k] = v
	}
}

// hostServSucceeded reads a labelled reply: a HostServ notice, or a batch
// of them.
func hostServSucceeded(reply *ircevent.Batch) bool {
	if reply == nil {
		return false
	}
	if reply.Command == "NOTICE" && len(reply.Params) >= 2 && strings.EqualFold(reply.Nick(), "HostServ") {
		return strings.HasPrefix(reply.Params[1], "Successfully")
	}
	for _, item := range reply.Items {
		if hostServSucceeded(item) {
			return true
		}
	}
	return false
}

// hostServSaid is what a labelled reply says, for the log.
func hostServSaid(reply *ircevent.Batch) string {
	if reply == nil {
		return "no answer"
	}
	var said []string
	if len(reply.Params) > 0 {
		said = append(said, reply.Command+" "+reply.Params[len(reply.Params)-1])
	}
	for _, item := range reply.Items {
		said = append(said, hostServSaid(item))
	}
	return strings.Join(said, "; ")
}

// refreshVhost gives nick its hostname again once its session has signed
// in. Ergo applies an account's hostname at sign-in and when it's set, to
// the sessions signed in by then; one set while a session is signing in
// can reach neither, so it's set again. A request already on its way is
// dropped too: its answer might say it worked.
func (r *Runner) refreshVhost(nick string) {
	k := strings.ToLower(nick)
	r.sync.vhostMu.Lock()
	if r.sync.vhosts == nil { // hostnames aren't synced yet; the first sync sets them all
		r.sync.vhostMu.Unlock()
		return
	}
	delete(r.sync.vhosts, k)
	delete(r.sync.asking, k)
	r.sync.vhostMu.Unlock()
	go r.resyncVhosts()
}

// resyncVhosts runs syncVhosts on its own, between full syncs.
func (r *Runner) resyncVhosts() {
	defer monitor.Recover()
	select {
	case <-r.stop:
		return
	default:
	}
	r.sync.mu.Lock()
	defer r.sync.mu.Unlock()
	r.syncVhosts()
}

// retryVhosts asks for refused hostnames again soon, then less often. Called
// with vhostMu held.
func (r *Runner) retryVhosts() {
	if r.sync.retryTimer != nil {
		return // one's already waiting
	}
	if r.sync.backoff == 0 {
		r.sync.backoff = vhostRetryFirst
	}
	wait := r.sync.backoff
	r.sync.backoff = min(2*wait, vhostRetryMax)
	r.sync.retryTimer = time.AfterFunc(wait, func() {
		r.sync.vhostMu.Lock()
		r.sync.retryTimer = nil
		r.sync.vhostMu.Unlock()
		r.resyncVhosts()
	})
}
