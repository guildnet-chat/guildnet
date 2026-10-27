// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/internal/workweek"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Approvals: when an agent wants to do something risky, it pauses and a
// person is asked. Who's asked, in order, is settled when it's raised (its
// route): the humans up the agent's reporting line who may approve, then the
// leads of its team and the teams it's inside, then the owner. If nobody
// answers for a while (4 working hours unless the owner says otherwise), it
// moves on to the next; anyone on the route can decide. The approval is
// stored, so it survives restarts and weekends; it expires after 24 working
// hours.

const (
	approvalWindow = 24 * time.Hour
	// moveAfterSetting is the org setting for how long an approval waits on
	// one person before moving on: a duration like "4h", or "off".
	moveAfterSetting = "approvals_move_after"
	defaultMoveAfter = 4 * time.Hour
)

// decision is a resume job: carry on after an approval was decided.
type decision struct {
	approval org.Approval
	allow    bool
	reason   string
}

// waitState is what the runner remembers about agents waiting on approvals.
type waitState struct {
	on     map[string]org.Approval // lowercase agent -> the approval it waits on
	parked map[string][]job        // wake-ups that arrived meanwhile
	told   map[string]bool         // agent+conversation already told it's waiting
	asking map[string]bool         // outside agents raising one now (agentAsk)
}

func newWaitState() waitState {
	return waitState{on: map[string]org.Approval{}, parked: map[string][]job{}, told: map[string]bool{}, asking: map[string]bool{}}
}

func (r *Runner) now() time.Time {
	if r.cfg.Clock != nil {
		return r.cfg.Clock()
	}
	return time.Now()
}

// waitingOn returns the approval an agent is waiting on, if any.
func (r *Runner) waitingOn(nick string) (org.Approval, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.wait.on[strings.ToLower(nick)]
	return a, ok
}

// park holds a wake-up until the agent's approval is decided, telling the
// conversation once that the agent is waiting.
func (r *Runner) park(j job, a org.Approval) {
	k := strings.ToLower(j.persona.Nick)
	r.mu.Lock()
	r.wait.parked[k] = append(r.wait.parked[k], j)
	told := r.wait.told[k+" "+strings.ToLower(j.key)]
	r.wait.told[k+" "+strings.ToLower(j.key)] = true
	r.mu.Unlock()
	if told {
		return
	}
	target := j.channel
	if target == "" {
		target = j.peer
	}
	r.ledgerConn.Privmsg(target, fmt.Sprintf("%s is waiting on approval #%d from %s and will pick this up after.",
		j.persona.Nick, a.ID, a.Approver))
}

// raiseApproval records that an agent paused before a risky action and asks
// the right person.
func (r *Runner) raiseApproval(p config.Persona, channel, asker string, req *agent.ApprovalRequest) (org.Approval, bool) {
	now := r.now()
	route := r.approvalRoute(p)
	if req.Tool == deskHostTool {
		route = []string{r.cfg.Owner} // only the owner chooses who desks reach
	}
	a := org.Approval{
		Agent: p.Nick, Approver: route[0], Route: route, Tool: req.Tool, Summary: req.Summary,
		Channel: channel, Asker: asker, Resume: req.Resume,
		RaisedAt: now, ExpiresAt: workweek.After(now, approvalWindow, r.loc()),
	}
	if req.Input != nil {
		// As the summary's hash names it: compact, keys in order.
		if b, err := json.Marshal(req.Input); err == nil {
			a.Input = string(b)
		}
	}
	if after := r.moveAfter(); after > 0 && len(route) > 1 {
		a.MovesAt = workweek.After(now, after, r.loc())
	}
	a.Resume = r.sealResume(a)
	a, err := r.store.RaiseApproval(context.Background(), a)
	if err != nil {
		monitor.Problem(r.logger, "couldn't record %s's approval request: %v", p.Nick, err)
		r.ledgerConn.Privmsg(r.cfg.OpsChannel, truncate(fmt.Sprintf("%s wanted to %s but the approval couldn't be recorded: %v", p.Nick, req.Summary, err), maxLineBytes))
		return a, false
	}
	r.mu.Lock()
	r.wait.on[strings.ToLower(p.Nick)] = a
	r.mu.Unlock()

	where := "a DM"
	if channel != "" {
		where = channel
	}
	r.guildConn.Privmsg(a.Approver, truncate(fmt.Sprintf("#%d: %s %s (for %s in %s). Reply APPROVE %d, or DENY %d <reason>. Expires %s.",
		a.ID, a.Agent, wants(a, shortSummary(a.Summary)), asker, where, a.ID, a.ID, a.ExpiresAt.In(r.loc()).Format("Mon 15:04 MST")), maxLineBytes))
	if channel != "" {
		r.ledgerConn.Privmsg(channel, truncate(fmt.Sprintf("approval #%d: %s %s; waiting on %s.", a.ID, a.Agent, wants(a, a.Summary), a.Approver), maxLineBytes))
	}
	r.logger.Printf("approval #%d: %s %s; asked %s", a.ID, a.Agent, wants(a, a.Summary), a.Approver)
	r.tellNews(approvalEvent(a, where, ""))
	return a, true
}

// approvalEvent is the news that approval a is waiting on its approver.
func approvalEvent(a org.Approval, where, movedFrom string) Event {
	return Event{Type: "approval", Approval: &ApprovalEvent{ID: a.ID, Agent: a.Agent, Summary: a.Summary, Where: where,
		Asker: a.Asker, Approver: a.Approver, MovedFrom: movedFrom, Expires: a.ExpiresAt, OwnWords: a.Tool == ownWords}}
}

// approvalRoute is who an agent's approval asks, in order: the humans up its
// reporting line who may approve for it, then the leads of its team and the
// teams it's inside who may, then the owner, last.
func (r *Runner) approvalRoute(p config.Persona) []string {
	ctx := context.Background()
	var route []string
	add := func(account string) {
		if r.isOwner(account) || slices.ContainsFunc(route, func(x string) bool { return strings.EqualFold(x, account) }) {
			return
		}
		if m, err := r.store.Member(ctx, account); err == nil && r.can(m.Account, canApprove, onPerson(p.Nick)) {
			route = append(route, m.Account)
		}
	}
	name, seen := p.ReportsTo, map[string]bool{}
	for name != "" && !seen[strings.ToLower(name)] {
		seen[strings.ToLower(name)] = true
		add(name)
		name = r.managerOf(name)
	}
	seen = map[string]bool{}
	for team := p.Team; team != "" && !seen[strings.ToLower(team)]; team = r.parentOf(team) {
		seen[strings.ToLower(team)] = true
		if t, err := r.store.Team(ctx, team); err == nil {
			for _, lead := range t.Leads {
				add(lead)
			}
		}
	}
	return append(route, r.cfg.Owner)
}

// moveAfter is how long an approval waits on one person before moving on;
// 0 means it doesn't.
func (r *Runner) moveAfter() time.Duration {
	v, _ := r.store.Setting(context.Background(), moveAfterSetting)
	if v == "off" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return defaultMoveAfter
}

// canDecide reports whether account may decide approval a: the owner, or
// anyone who may approve for the agent now (by default, anyone above it who
// leads or manages). Its route (approvalRoute) is only who held that when
// it was raised: being on it, or being asked, is no say once that's gone.
func (r *Runner) canDecide(account string, a org.Approval) bool {
	if r.isOwner(account) {
		return true
	}
	if a.Tool == deskHostTool {
		return false // only the owner chooses who desks reach
	}
	// Only someone still in the org: one taken out since keeps no say.
	if !r.inOrgAsPerson(account) {
		return false
	}
	return r.can(account, canApprove, onPerson(a.Agent))
}

// moveApprovals asks the next person on each approval that's waited long
// enough on the one before.
func (r *Runner) moveApprovals() {
	ctx := context.Background()
	pending, err := r.store.PendingApprovals(ctx)
	if err != nil {
		return
	}
	now := r.now()
	for _, a := range pending {
		if a.MovesAt.IsZero() || now.Before(a.MovesAt) {
			continue
		}
		i := slices.IndexFunc(a.Route, func(x string) bool { return strings.EqualFold(x, a.Approver) })
		if i < 0 || i+1 >= len(a.Route) {
			r.store.MoveApproval(ctx, a.ID, a.Approver, time.Time{}) // nobody left to ask: it stays
			continue
		}
		next, movesAt := a.Route[i+1], time.Time{}
		if after := r.moveAfter(); after > 0 && i+2 < len(a.Route) {
			movesAt = workweek.After(now, after, r.loc())
		}
		if err := r.store.MoveApproval(ctx, a.ID, next, movesAt); err != nil {
			continue // decided meanwhile
		}
		before := a.Approver
		a.Approver, a.MovesAt = next, movesAt
		r.mu.Lock()
		if w, ok := r.wait.on[strings.ToLower(a.Agent)]; ok && w.ID == a.ID {
			r.wait.on[strings.ToLower(a.Agent)] = a
		}
		r.mu.Unlock()
		r.guildConn.Privmsg(next, truncate(fmt.Sprintf("#%d, moved to you: %s didn't answer. %s %s (for %s). Reply APPROVE %d, or DENY %d <reason>. Expires %s.",
			a.ID, before, a.Agent, wants(a, shortSummary(a.Summary)), a.Asker, a.ID, a.ID, a.ExpiresAt.In(r.loc()).Format("Mon 15:04 MST")), maxLineBytes))
		r.logChange("", "move approval", a.Agent, r.teamOf(a.Agent), fmt.Sprintf("#%d from %s to %s", a.ID, before, next))
		r.logger.Printf("approval #%d moved from %s to %s", a.ID, before, next)
		where := "a DM"
		if a.Channel != "" {
			where = a.Channel
		}
		r.tellNews(approvalEvent(a, where, before))
	}
}

// setMoveAfter sets how long approvals wait on one person, as actor (the
// owner): a duration like "4h" or "30m", or "off".
func (r *Runner) setMoveAfter(actor, value string) (time.Duration, error) {
	if !r.isOwner(actor) {
		return 0, refuse(forbidden, "only %s (the owner) sets how long approvals wait.", r.cfg.Owner)
	}
	value = strings.ToLower(strings.TrimSpace(value))
	var d time.Duration
	if value != "off" {
		var err error
		d, err = time.ParseDuration(value)
		if err != nil || d < time.Minute || d > approvalWindow {
			return 0, refuse(invalid, "say how long, from 1m to 24h, like 4h or 30m; or off.")
		}
		value = shortDuration(d)
	}
	if err := r.store.SetSetting(context.Background(), moveAfterSetting, value); err != nil {
		return 0, r.storeRefusal("the setting", err)
	}
	r.logChange(actor, "set approvals", "", "", "move on after "+value)
	return d, nil
}

// shortDuration is "4h", "30m", or "1h30m".
func shortDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case m == 0:
		return fmt.Sprintf("%dh", h)
	case h == 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// guildDecide runs APPROVE <id> [note] and DENY <id> <reason>.
func (r *Runner) guildDecide(from, actor string, allow bool, args []string) {
	verb := "APPROVE"
	if !allow {
		verb = "DENY"
	}
	if len(args) == 0 {
		r.guildReply(from, fmt.Sprintf("usage: %s <number> [reason]", verb))
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil {
		r.guildReply(from, fmt.Sprintf("usage: %s <number> [reason]", verb))
		return
	}
	reason := strings.TrimSpace(strings.Join(args[1:], " "))
	a, err := r.decideApproval(actor, id, allow, reason)
	if err != nil {
		r.guildReply(from, err.Error())
		return
	}
	r.guildReply(from, fmt.Sprintf("%s #%d; %s has been told.", a.Status, id, a.Agent))
}

// resumeAfter queues an agent to carry on after its approval was decided.
func (r *Runner) resumeAfter(a org.Approval, allow bool, reason string) {
	p, ok := r.persona(a.Agent)
	if !ok {
		// Fired meanwhile: there's no one to carry on.
		r.store.ClaimResume(context.Background(), a.ID, r.now())
		return
	}
	// One job per decision at a time, however it came (deciding, expiring,
	// or replayDecisions after a restart).
	r.mu.Lock()
	if r.resuming[a.ID] {
		r.mu.Unlock()
		return
	}
	if r.resuming == nil {
		r.resuming = map[int64]bool{}
	}
	r.resuming[a.ID] = true
	r.mu.Unlock()
	// Lists of approvals leave out what the agent carries on from.
	if a.Resume == "" {
		if full, err := r.store.Approval(context.Background(), a.ID); err == nil {
			a.Resume = full.Resume
		}
	}
	token, _, err := r.openResume(a)
	if err != nil {
		// Nothing safe to carry on from: it's done with, and they're told.
		monitor.Problem(r.logger, "%s can't carry on after #%d: %v", a.Agent, a.ID, err)
		r.store.ClaimResume(context.Background(), a.ID, r.now())
		r.mu.Lock()
		delete(r.resuming, a.ID)
		r.mu.Unlock()
		target := a.Channel
		if target == "" {
			target = a.Asker
		}
		r.ledgerConn.Privmsg(target, fmt.Sprintf("%s can't carry on after #%d: %v.", a.Agent, a.ID, err))
		return
	}
	a.Resume = token
	if !r.enqueue(job{persona: p, key: fmt.Sprintf("approval #%d", a.ID), decision: &decision{approval: a, allow: allow, reason: reason}}) {
		// The agent's queue is full: replayDecisions tries again.
		r.mu.Lock()
		delete(r.resuming, a.ID)
		r.mu.Unlock()
	}
}

// claimResume marks the decision j carries as carried on with, and reports
// whether to go on: no one has, or will.
func (r *Runner) claimResume(a org.Approval) bool {
	claimed, err := r.store.ClaimResume(context.Background(), a.ID, r.now())
	r.mu.Lock()
	delete(r.resuming, a.ID)
	r.mu.Unlock()
	if err != nil {
		monitor.Problem(r.logger, "carrying on after #%d: %v", a.ID, err)
		return false // replayDecisions tries again
	}
	return claimed
}

// replayDecisions carries on after decisions no agent has yet: one made
// just before a restart, or that found its agent's queue full.
func (r *Runner) replayDecisions() {
	decided, err := r.store.Unresumed(context.Background())
	if err != nil {
		monitor.Problem(r.logger, "decisions to carry on with: %v", err)
		return
	}
	for _, a := range decided {
		r.resumeAfter(a, a.Status == org.Approved, a.Reason)
	}
}

// handleDecision carries an agent on after a decision and delivers what it
// says to where it was working.
func (r *Runner) handleDecision(p config.Persona, d decision, act *activity) {
	a := d.approval
	ctx, cancel := context.WithTimeout(context.Background(), r.timeoutFor(p))
	defer cancel()
	// What was approved runs whatever's left of the budget (a person said
	// so); the agent carrying on after it is held to it like any wake-up.
	m := r.meter(p, "approval")
	ctx = agent.WithMeter(ctx, m)
	var res agent.Result
	var err error
	if p.Platform == PlatformOutside {
		res, err = r.resumeOutside(ctx, p, d, act.step)
	} else {
		res, err = r.brain.Resume(ctx, p, a.Resume, d.allow, d.reason, act.step)
	}
	act.summarize()
	cost := m.settle(res)
	if err != nil {
		monitor.Problem(r.logger, "%s couldn't carry on after #%d: %v", p.Nick, a.ID, err)
	}
	target, key := a.Channel, a.Channel
	if target == "" {
		target, key = a.Asker, chat.DMKey(p.Nick, a.Asker)
	}
	if res.Reply != "" && res.Reply != agent.Pass {
		r.deliver(p, a.Channel, target, key, res.Reply, "")
	}
	verdict := "approved"
	if !d.allow {
		verdict = "denied"
	}
	asked := fmt.Sprintf("carrying on after #%d was %s: %s", a.ID, verdict, a.Summary)
	if res.Approval != nil { // straight on to the next risky step
		next, ok := r.raiseApproval(p, a.Channel, a.Asker, res.Approval)
		var id int64
		if ok {
			id = next.ID
		}
		r.keepWork(p, act, a.Channel, a.DecidedBy, asked, outcomeOf(res, err, id), cost)
		return
	}
	r.keepWork(p, act, a.Channel, a.DecidedBy, asked, outcomeOf(res, err, 0), cost)
	r.stopWaiting(p.Nick)
}

// stopWaiting clears an agent's wait and requeues what was parked.
func (r *Runner) stopWaiting(nick string) {
	k := strings.ToLower(nick)
	r.mu.Lock()
	delete(r.wait.on, k)
	parked := r.wait.parked[k]
	delete(r.wait.parked, k)
	for t := range r.wait.told {
		if strings.HasPrefix(t, k+" ") {
			delete(r.wait.told, t)
		}
	}
	r.mu.Unlock()
	r.staffMu.RLock()
	conn := r.conns[k]
	r.staffMu.RUnlock()
	if conn != nil {
		conn.Send("AWAY") // no longer waiting
	}
	for _, j := range parked {
		r.enqueue(j)
	}
}

// dropApprovals denies what a fired agent was waiting on, as by, and
// forgets its wait and what was parked behind it: an agent hired later with
// the same nick starts afresh, and approving the old one runs nothing.
func (r *Runner) dropApprovals(nick, by string) {
	ctx := context.Background()
	if pending, err := r.store.PendingApprovals(ctx); err == nil {
		for _, a := range pending {
			if strings.EqualFold(a.Agent, nick) {
				if err := r.store.DecideApproval(ctx, a.ID, org.Denied, by, nick+" was fired", r.now()); err != nil {
					monitor.Problem(r.logger, "denying #%d of fired %s: %v", a.ID, nick, err)
				}
			}
		}
	}
	k := strings.ToLower(nick)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.wait.on, k)
	delete(r.wait.parked, k)
	for t := range r.wait.told {
		if strings.HasPrefix(t, k+" ") {
			delete(r.wait.told, t)
		}
	}
}

// expireApprovals denies approvals nobody decided within 24 working hours.
func (r *Runner) expireApprovals() {
	ctx := context.Background()
	pending, err := r.store.PendingApprovals(ctx)
	if err != nil {
		return
	}
	now := r.now()
	for _, a := range pending {
		if now.Before(a.ExpiresAt) {
			continue
		}
		reason := "no decision within 24 working hours, so it wasn't done"
		if err := r.store.DecideApproval(ctx, a.ID, org.Expired, "", reason, now); err != nil {
			continue
		}
		r.guildConn.Privmsg(a.Approver, truncate(fmt.Sprintf("#%d expired without a decision: %s won't %s.", a.ID, a.Agent, a.Summary), maxLineBytes))
		r.ledgerConn.Privmsg(r.cfg.OpsChannel, truncate(fmt.Sprintf("#%d expired (%s: %s)", a.ID, a.Agent, a.Summary), maxLineBytes))
		r.resumeAfter(a, false, reason)
	}
}

// watchApprovals restores waits after a restart and expires old approvals.
func (r *Runner) watchApprovals(stop <-chan struct{}) {
	defer monitor.Recover()
	r.sealKept()
	if pending, err := r.store.PendingApprovals(context.Background()); err == nil {
		r.mu.Lock()
		for _, a := range pending {
			r.wait.on[strings.ToLower(a.Agent)] = a
		}
		r.mu.Unlock()
	}
	r.replayDecisions()
	every := r.cfg.ApprovalCheckEvery
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.moveApprovals()
			r.expireApprovals()
			r.replayDecisions()
		}
	}
}

// waitingOnYou lists the pending approvals account is asked to decide.
func (r *Runner) waitingOnYou(account string) []string {
	pending, _ := r.store.PendingApprovals(context.Background())
	var out []string
	for _, a := range pending {
		if strings.EqualFold(a.Approver, account) {
			out = append(out, fmt.Sprintf("Waiting on you: #%d %s %s (APPROVE %d / DENY %d, expires %s).",
				a.ID, a.Agent, wants(a, a.Summary), a.ID, a.ID, a.ExpiresAt.In(r.loc()).Format("Mon 15:04 MST")))
		}
	}
	return out
}

// guildApprovals runs APPROVALS: what's waiting that you may decide; and
// APPROVALS AFTER <4h|30m|off>: how long one waits on each person.
func (r *Runner) guildApprovals(from, actor string, args []string) {
	if len(args) > 0 {
		if !strings.EqualFold(args[0], "AFTER") || len(args) != 2 {
			r.guildReply(from, "usage: APPROVALS, or APPROVALS AFTER <how long, like 4h or 30m | off>.")
			return
		}
		d, err := r.setMoveAfter(actor, args[1])
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		if d == 0 {
			r.guildReply(from, "approvals now stay with the first person asked until they're decided or expire.")
			return
		}
		r.guildReply(from, fmt.Sprintf("an approval nobody answers now moves on to the next person after %s of working time.", shortDuration(d)))
		return
	}
	pending := r.approvalsFor(actor)
	for _, a := range pending {
		r.guildReply(from, fmt.Sprintf("#%d %s %s (asked of %s, expires %s).",
			a.ID, a.Agent, wants(a, a.Summary), a.Approver, a.ExpiresAt.In(r.loc()).Format("Mon 15:04 MST")))
	}
	if len(pending) == 0 {
		r.guildReply(from, "nothing is waiting on you.")
	}
}

// ownWords is the tool an outside agent's approval in its own words is
// raised as (request_approval): nothing pins it to an action.
const ownWords = "request_approval"

// wants says what approval a is for, with summary (its words, perhaps cut
// short): as the tool described the input that will run, or, asked in an
// outside agent's own words, marked as theirs, so nobody takes it for a
// description GuildNet checked.
func wants(a org.Approval, summary string) string {
	if a.Tool == ownWords {
		return "asks, in its own words, to “" + summary + "”"
	}
	return "wants to " + summary
}

// shortSummary is what an agent wants to do, short enough that GuildServ's
// question keeps its own closing words (chat's buttons go by them).
func shortSummary(s string) string { return truncate(s, 200) }
