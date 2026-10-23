// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// The org's rules, in one place: who may hire, change, and fire agents, team
// budgets, reporting lines, and deciding approvals. GuildServ (IRC) and the
// runner's API both go through these, so a rule is written once and holds
// the same everywhere.

// refusalKind is why a request was turned down.
type refusalKind int

const (
	invalid   refusalKind = iota // the request doesn't make sense
	forbidden                    // the person asking may not do this
	notFound                     // what it names doesn't exist
	conflict                     // it's already done, or the name is taken
	failed                       // something broke on our side
	tooMany                      // too many at once: wait a moment
)

// refusal is a request the org's rules turned down. Its message is for the
// person who asked.
type refusal struct {
	kind refusalKind
	msg  string
}

func (e *refusal) Error() string { return e.msg }

func refuse(kind refusalKind, format string, a ...any) error {
	return &refusal{kind: kind, msg: fmt.Sprintf(format, a...)}
}

// kindOf is err's refusal kind; anything else counts as failed.
func kindOf(err error) refusalKind {
	var rf *refusal
	if errors.As(err, &rf) {
		return rf.kind
	}
	return failed
}

// storeRefusal turns a store error about name into a refusal.
func (r *Runner) storeRefusal(name string, err error) error {
	switch {
	case errors.Is(err, org.ErrExists):
		return refuse(conflict, "%s is already in the org.", name)
	case errors.Is(err, org.ErrNotFound):
		return refuse(notFound, "%s isn't in the org.", name)
	}
	monitor.Problem(r.logger, "org store: %v", err)
	return refuse(failed, "that didn't work: the staff database had a problem. Try again.")
}

// field is one profile field to set, by its GuildServ name (ROLE, MODEL, ...).
type field struct{ name, value string }

// inOrgAsPerson reports whether account is a person in the org (the owner or
// a member), as opposed to an agent, a bot, or a stranger.
func (r *Runner) inOrgAsPerson(account string) bool {
	if account == "" || r.isAgent(account) || isService(account) || strings.EqualFold(account, r.cfg.LedgerNick) {
		return false
	}
	if r.isOwner(account) {
		return true
	}
	_, err := r.store.Member(context.Background(), account)
	return err == nil
}

func (r *Runner) isGuest(account string) bool {
	m, err := r.store.Member(context.Background(), account)
	return err == nil && m.Guest
}

// mayCatchUp reports whether account gets catch-ups, each a model call on
// the company's budget: the company's people do, and guests once the owner
// lets them use agents.
func (r *Runner) mayCatchUp(account string) bool {
	if !r.inOrgAsPerson(account) {
		return false
	}
	return !r.isGuest(account) || r.guestsAsk(context.Background())
}

// mayAsk reports whether account may have agent p work for them: p works
// with its team's repos and the company's connected services for whoever
// asks, and answers them. A person in the org may; a guest only once the
// owner lets guests use agents (COMPANY GUESTS on); and for an agent in a
// private team, or a team inside one, only someone in that team's channel
// (its people, and the leads above it) or the owner.
func (r *Runner) mayAsk(account string, p config.Persona) bool {
	if !r.inOrgAsPerson(account) {
		return false
	}
	if r.isOwner(account) {
		return true
	}
	ctx := context.Background()
	if m, err := r.store.Member(ctx, account); err == nil && m.Guest && !r.guestsAsk(ctx) {
		return false
	}
	return r.seesTeam(account, p.Team)
}

// seesTeam reports whether account may see team's details (its leads,
// budget, repositories, connectors, objectives) and have its agents work
// for them: for a private team, or a team inside one, only someone in that
// private team's channel (its people, and the leads above it) or the
// owner. Anyone may see that a private team is there, and that it's
// private. "" (the company itself) is everyone's.
func (r *Runner) seesTeam(account, team string) bool {
	if r.isOwner(account) {
		return true
	}
	ctx := context.Background()
	var plans map[string]*channelPlan
	seen := map[string]bool{}
	for team != "" && !seen[strings.ToLower(team)] {
		seen[strings.ToLower(team)] = true
		t, err := r.store.Team(ctx, team)
		if err != nil {
			return false
		}
		if t.Private {
			if plans == nil {
				plans = r.plan()
			}
			plan := plans[strings.ToLower(t.Channel)]
			if plan == nil {
				return false
			}
			if _, in := plan.ranks[strings.ToLower(account)]; !in {
				return false
			}
		}
		team = t.Parent
	}
	return true
}

// guestsAskSetting is set ("on") when the owner lets guests have agents
// work for them.
const guestsAskSetting = "guests-ask-agents"

// guestsAsk reports whether the owner lets guests have agents work for them.
func (r *Runner) guestsAsk(ctx context.Context) bool {
	v, _ := r.store.Setting(ctx, guestsAskSetting)
	return v != ""
}

// setGuestsAsk lets guests have agents work for them, or not: the owner's
// call, since agents work with the company's accounts.
func (r *Runner) setGuestsAsk(actor string, on bool) error {
	if !r.isOwner(actor) {
		return refuse(forbidden, "only the owner decides whether guests may have agents work for them.")
	}
	v := ""
	if on {
		v = "on"
	}
	if err := r.store.SetSetting(context.Background(), guestsAskSetting, v); err != nil {
		return r.storeRefusal("the company", err)
	}
	r.logChange(actor, "set company", "", "", fmt.Sprintf("guests ask agents: %t", on))
	return nil
}

// hireAgent hires an agent as actor. notes are worth passing on (a budget
// warning, a platform hiccup).
func (r *Runner) hireAgent(actor, nick string, fields []field) (p config.Persona, notes []string, err error) {
	if msg := r.checkNewNick(nick); msg != "" {
		return p, nil, refuse(invalid, "%s", msg)
	}
	p = config.Persona{
		Nick:           nick,
		Name:           strings.ToUpper(nick[:1]) + nick[1:],
		Role:           "Team member",
		Model:          r.defaultModel(),
		Effort:         defaultEffort,
		DailyBudgetUSD: defaultBudget,
		Channels:       append([]string(nil), defaultChannels...),
	}
	// A lead hires into a team they lead unless they say otherwise.
	if !r.isOwner(actor) {
		if led := r.teamsLedBy(actor); len(led) > 0 {
			p.Team = led[0]
		}
	}
	explicit := map[string]bool{}
	for _, f := range presetFirst(fields) {
		if msg := r.applyField(&p, f.name, f.value); msg != "" {
			return p, nil, refuse(invalid, "%s", msg)
		}
		explicit[word(f.name)] = true
	}
	if msg := deskProblem(p); msg != "" {
		return p, nil, refuse(invalid, "%s", msg)
	}
	if err := r.allowed(actor, canHire, where(p.Team), "hire"); err != nil {
		return p, nil, err
	}
	if err := r.mayPutAgentIn(actor, r.channelsFor(p)); err != nil {
		return p, nil, err
	}
	if explicit["BUDGET"] {
		if err := r.allowed(actor, canBudget, where(p.Team), "set agents' budgets"); err != nil {
			return p, nil, err
		}
	}
	refusal, warning := r.checkTeamBudget(actor, p.Team, nick, p.DailyBudgetUSD)
	if refusal != "" {
		return p, nil, refuse(forbidden, "%s", refusal)
	}
	newcomer := target{person: nick, team: p.Team}
	if explicit["REPORTS-TO"] {
		if err := r.mayMakeManager(actor, p.ReportsTo, newcomer); err != nil {
			return p, nil, err
		}
	} else if p.ReportsTo = r.defaultManager(actor, p.Team, nick); r.mayMakeManager(actor, p.ReportsTo, newcomer) != nil {
		p.ReportsTo = r.cfg.Owner // hiring alone doesn't make actor its manager
	}
	if err := r.claimAccount(nick); err != nil {
		return p, nil, err
	}
	if err := r.store.Hire(context.Background(), p); err != nil {
		return p, nil, r.storeRefusal(nick, err)
	}
	if note := r.tellPlatform("hire", p); note != "" {
		notes = append(notes, note)
	}
	if err := r.startAgent(p); err != nil {
		monitor.Problem(r.logger, "hired %s but couldn't connect: %v", nick, err)
		notes = append(notes, fmt.Sprintf("%s couldn't connect yet: %v", nick, err))
	}
	r.requestSync()
	r.logger.Printf("%s hired %s (%s)", actor, nick, p.Role)
	r.logChange(actor, "hire", p.Nick, p.Team, fmt.Sprintf("%s, reporting to %s, %s", p.Role, p.ReportsTo, r.spendNote(p)))
	if warning != "" {
		notes = append(notes, warning)
	}
	return p, notes, nil
}

// setAgent changes an agent's profile as actor.
func (r *Runner) setAgent(actor, nick string, fields []field) (p config.Persona, notes []string, err error) {
	p, err = r.store.Agent(context.Background(), nick)
	if err != nil {
		return p, nil, r.storeRefusal(nick, err)
	}
	if err := r.allowed(actor, canHire, onPerson(p.Nick), "change agents"); err != nil {
		return p, nil, err
	}
	before := p
	budgetChanged, budgetSet := false, false
	for _, f := range presetFirst(fields) {
		name := word(f.name)
		if msg := r.applyField(&p, f.name, f.value); msg != "" {
			return p, nil, refuse(invalid, "%s", msg)
		}
		switch name {
		case "TEAM":
			if err := r.allowed(actor, canHire, where(p.Team), "move agents"); err != nil {
				return p, nil, err
			}
			budgetChanged = true
		case "BUDGET":
			if err := r.allowed(actor, canBudget, onPerson(p.Nick), "set agents' budgets"); err != nil {
				return p, nil, err
			}
			budgetChanged, budgetSet = true, true
		}
	}
	// A preset brings its own budget.
	if p.DailyBudgetUSD != before.DailyBudgetUSD && !budgetSet {
		if err := r.allowed(actor, canBudget, onPerson(p.Nick), "set agents' budgets"); err != nil {
			return p, nil, err
		}
		budgetChanged = true
	}
	if !strings.EqualFold(p.ReportsTo, before.ReportsTo) {
		if err := r.mayMakeManager(actor, p.ReportsTo, target{person: p.Nick, team: p.Team}); err != nil {
			return p, nil, err
		}
	}
	if msg := deskProblem(p); msg != "" {
		return p, nil, refuse(invalid, "%s", msg)
	}
	if err := r.mayPutAgentIn(actor, added(r.channelsFor(before), r.channelsFor(p))); err != nil {
		return p, nil, err
	}
	if budgetChanged {
		refusal, warning := r.checkTeamBudget(actor, p.Team, p.Nick, p.DailyBudgetUSD)
		if refusal != "" {
			return p, nil, refuse(forbidden, "%s", refusal)
		}
		if warning != "" {
			notes = append(notes, warning)
		}
	}
	if err := r.store.Update(context.Background(), p); err != nil {
		return p, nil, r.storeRefusal(nick, err)
	}
	// Moved off its desk, or to another team at it: the desk is cleared, so
	// nothing of it (the old team's repos, what was left in it) carries over.
	if before.Platform != p.Platform || (before.Platform == PlatformDesk && !strings.EqualFold(before.Team, p.Team)) {
		if note := r.tellPlatform("fire", before); note != "" {
			notes = append(notes, note)
		}
	}
	// Brought back from outside: its token and webhook go, so sent outside
	// again it needs a new token, and an old one that leaked stays dead.
	if before.Platform == PlatformOutside && p.Platform != PlatformOutside {
		r.endOutside(p.Nick)
	}
	if note := r.tellPlatform("update", p); note != "" {
		notes = append(notes, note)
	}
	r.updateAgent(p)
	r.requestSync()
	var names []string
	for _, f := range fields {
		names = append(names, word(f.name))
	}
	r.logger.Printf("%s set %s %s", actor, p.Nick, strings.Join(names, ","))
	r.logChange(actor, "set agent", p.Nick, p.Team, changes(names, func(name string) (string, string) {
		return agentField(before, name), agentField(p, name)
	}, "PROMPT"))
	return p, notes, nil
}

// agentField is one of an agent's fields, as the log shows it.
func agentField(p config.Persona, name string) string {
	switch name {
	case "NAME":
		return p.Name
	case "ROLE":
		return p.Role
	case "TEAM":
		return p.Team
	case "REPORTS-TO":
		return p.ReportsTo
	case "MODEL":
		return p.Model
	case "EFFORT":
		return p.Effort
	case "CHANNELS":
		return strings.Join(p.Channels, ",")
	case "BUDGET":
		return usd(p.DailyBudgetUSD) + "/day"
	case "PLATFORM":
		return p.Platform
	case "PROMPT":
		return p.Prompt // compared, never shown: too long
	}
	return ""
}

// changes describes a change field by field: "role Engineer → Staff
// engineer; prompt changed". Fields that didn't change (a form sends them
// all) are left out; hidden ones (a prompt, what someone costs) only say
// that they changed.
func changes(names []string, values func(name string) (before, after string), hidden ...string) string {
	var parts []string
	for _, n := range names {
		b, a := values(n)
		switch {
		case b == a:
			continue
		case slices.Contains(hidden, n):
			parts = append(parts, strings.ToLower(n)+" changed")
		default:
			if b == "" {
				b = "none"
			}
			if a == "" {
				a = "none"
			}
			parts = append(parts, fmt.Sprintf("%s %s → %s", strings.ToLower(n), b, a))
		}
	}
	return strings.Join(parts, "; ")
}

// lockOut takes someone removed from the org off IRC: their account is
// suspended, which disconnects every session of it (the always-on one too)
// and stops it signing in, whatever app they use and whoever changed their
// password. letIn undoes it when they're added again.
func (r *Runner) lockOut(account string) {
	r.guildConn.Privmsg("NickServ", "SUSPEND ADD "+account+" removed from the org")
}

func (r *Runner) letIn(account string) {
	r.guildConn.Privmsg("NickServ", "SUSPEND DEL "+account)
}

// endIRCSessions ends every IRC session of account's, as actor: they
// themselves (an app password revoked, a laptop lost) or the owner. It's
// suspended and let in again at once: Ergo disconnects a suspended account.
func (r *Runner) endIRCSessions(actor, account string) error {
	m, err := r.store.Member(context.Background(), account)
	if err != nil {
		return r.storeRefusal(account, err)
	}
	if !strings.EqualFold(actor, m.Account) && !r.isOwner(actor) {
		return refuse(forbidden, "only %s, or the owner, can end %s's IRC sessions.", m.Account, m.Account)
	}
	r.guildConn.Privmsg("NickServ", "SUSPEND ADD "+m.Account+" its IRC sessions ended")
	r.letIn(m.Account)
	r.logChange(actor, "end irc sessions", m.Account, m.Team, "")
	return nil
}

// endOutside ends the token and webhook nick had outside GuildNet: its
// token becomes one nobody was given, and wake-ups go back to the inbox.
func (r *Runner) endOutside(nick string) {
	ctx := context.Background()
	if err := r.store.SetAgentToken(ctx, nick, hashToken("ended_"+randomID()+randomID())); err != nil && !errors.Is(err, org.ErrNotFound) {
		monitor.Problem(r.logger, "ending %s's token: %v", nick, err)
	}
	if err := r.store.SetAgentWebhook(ctx, nick, "", nil); err != nil && !errors.Is(err, org.ErrNotFound) {
		monitor.Problem(r.logger, "ending %s's webhook: %v", nick, err)
	}
}

// fireAgent fires an agent as actor; reason is shown as it leaves.
func (r *Runner) fireAgent(actor, nick, reason string) error {
	p, err := r.store.Agent(context.Background(), nick)
	if err != nil {
		return r.storeRefusal(nick, err)
	}
	if err := r.allowed(actor, canHire, onPerson(p.Nick), "fire agents"); err != nil {
		return err
	}
	if err := r.store.Fire(context.Background(), nick); err != nil {
		return r.storeRefusal(nick, err)
	}
	quit, record := "Fired by "+actor, actor+" fired "+p.Nick
	if reason != "" {
		quit += ": " + reason
		record += ": " + reason
	}
	r.dropApprovals(p.Nick, actor)
	r.tellPlatform("fire", p)
	r.stopAgent(p.Nick, quit)
	r.requestSync()
	r.logger.Print(record)
	r.logChange(actor, "fire", p.Nick, p.Team, reason)
	r.ledgerConn.Privmsg(r.cfg.OpsChannel, truncate(record, maxLineBytes))
	return nil
}

// approvalsFor is what's waiting that actor may decide.
func (r *Runner) approvalsFor(actor string) []org.Approval {
	pending, _ := r.store.PendingApprovals(context.Background())
	var out []org.Approval
	for _, a := range pending {
		if r.canDecide(actor, a) {
			out = append(out, a)
		}
	}
	return out
}

// decideApproval approves or denies approval id as actor, and lets the agent
// carry on.
func (r *Runner) decideApproval(actor string, id int64, allow bool, reason string) (org.Approval, error) {
	ctx := context.Background()
	a, err := r.store.Approval(ctx, id)
	if err != nil {
		return a, refuse(notFound, "there's no approval #%d.", id)
	}
	if !r.canDecide(actor, a) {
		return a, refuse(forbidden, "only %s, someone above them who may approve, or the owner can decide #%d.", a.Approver, id)
	}
	status := org.Approved
	if !allow {
		status = org.Denied
	}
	if err := r.store.DecideApproval(ctx, id, status, actor, reason, r.now()); err != nil {
		if errors.Is(err, org.ErrDecided) {
			cur, _ := r.store.Approval(ctx, id)
			return cur, refuse(conflict, "#%d was already %s.", id, cur.Status)
		}
		monitor.Problem(r.logger, "deciding #%d: %v", id, err)
		return a, refuse(failed, "that didn't work: the database had a problem. Try again.")
	}
	record := fmt.Sprintf("%s %s #%d (%s: %s)", actor, status, id, a.Agent, a.Summary)
	if reason != "" {
		record += ": " + reason
	}
	r.ledgerConn.Privmsg(r.cfg.OpsChannel, truncate(record, maxLineBytes))
	detail := fmt.Sprintf("#%d %s", id, a.Summary)
	if reason != "" {
		detail += ": " + reason
	}
	r.logChange(actor, strings.TrimSuffix(status, "d"), a.Agent, r.teamOf(a.Agent), detail)
	a.Status, a.DecidedBy, a.Reason = status, actor, reason
	if a.Tool == deskHostTool {
		r.hostDecided(actor, a, allow, reason)
	}
	r.resumeAfter(a, allow, reason)
	return a, nil
}

// tellPlatform passes a staffing change on to the agent platform, if it keeps
// its own copy of agents, and returns a note for whoever made the change if
// that failed.
func (r *Runner) tellPlatform(change string, p config.Persona) string {
	s, ok := r.brain.(Staffing)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	switch change {
	case "hire":
		err = s.Hire(ctx, p)
	case "update":
		err = s.Update(ctx, p)
	case "fire":
		err = s.Fire(ctx, p)
	}
	if err != nil {
		monitor.Problem(r.logger, "agent platform %s %s: %v", change, p.Nick, err)
		return fmt.Sprintf("note: the agent platform couldn't %s %s yet (%v); it'll be retried.", change, p.Nick, err)
	}
	return ""
}

// createTeam creates a team as actor: at the top of the org, or inside a
// team they may arrange.
func (r *Runner) createTeam(actor, name string, fields []field) (org.Team, error) {
	l := org.Team{Name: strings.TrimSpace(name), Channel: org.TeamChannel(name)}
	if l.Name == "" {
		return l, refuse(invalid, "a team needs a name.")
	}
	if _, err := r.store.Team(context.Background(), l.Name); err == nil {
		return l, refuse(conflict, "there's already a team %s.", l.Name)
	}
	var leads, budget bool
	for _, f := range fields {
		if msg := r.applyTeamField(&l, f.name, f.value); msg != "" {
			return l, refuse(invalid, "%s", msg)
		}
		switch word(f.name) {
		case "LEAD", "LEADS":
			leads = true
		case "BUDGET":
			budget = true
		}
	}
	if err := r.allowed(actor, canArrange, where(l.Parent), "create teams"); err != nil {
		return l, err
	}
	if err := r.mayShareFromPrivateTeams(actor, org.Team{Name: l.Name}, l); err != nil {
		return l, err
	}
	if msg := r.checkTeamChannel(l.Name, l.Channel); msg != "" {
		return l, refuse(invalid, "%s", msg)
	}
	if err := r.mayTakeChannel(actor, l.Channel); err != nil {
		return l, err
	}
	if leads {
		if err := r.mayLead(actor, where(l.Parent)); err != nil {
			return l, err
		}
	}
	var warning string
	if budget {
		w, err := r.checkTeamBudgetChange(actor, l)
		if err != nil {
			return l, err
		}
		warning = w
	}
	if err := r.store.CreateTeam(context.Background(), l); err != nil {
		return l, r.storeRefusal(name, err)
	}
	r.requestSync()
	r.setFocusTopic(l)
	r.logger.Printf("%s created team %s", actor, l.Name)
	r.logChange(actor, "create team", l.Name, l.Name, describeTeam(l))
	if warning != "" {
		r.logger.Print(warning)
	}
	return l, nil
}

// setTeam changes a team as actor.
func (r *Runner) setTeam(actor, name string, fields []field) (org.Team, error) {
	l, err := r.store.Team(context.Background(), name)
	if err != nil {
		return l, r.storeRefusal(name, err)
	}
	before := l
	var names []string
	for _, f := range fields {
		n := word(f.name)
		if n == "LEAD" {
			n = "LEADS"
		}
		names = append(names, n)
		if msg := r.applyTeamField(&l, f.name, f.value); msg != "" {
			return l, refuse(invalid, "%s", msg)
		}
		switch n {
		case "LEADS":
			if err := r.mayLead(actor, inTeam(l.Name)); err != nil {
				return l, err
			}
			for _, gone := range before.Leads {
				if !l.LedBy(gone) && !r.isOwner(actor) && (strings.EqualFold(gone, actor) || r.below(actor, gone)) {
					return l, refuse(forbidden, "you can't stop yourself or anyone above you leading %s.", l.Name)
				}
			}
		case "PARENT":
			if err := r.allowed(actor, canArrange, where(before.Parent), "move teams"); err != nil {
				return l, err
			}
			if err := r.allowed(actor, canArrange, where(l.Parent), "move teams"); err != nil {
				return l, err
			}
		case "BUDGET":
			if _, err := r.checkTeamBudgetChange(actor, l); err != nil {
				return l, err
			}
		default:
			if err := r.allowed(actor, canArrange, inTeam(l.Name), "change teams"); err != nil {
				return l, err
			}
		}
	}
	if !strings.EqualFold(l.Channel, before.Channel) {
		if err := r.mayTakeChannel(actor, l.Channel); err != nil {
			return l, err
		}
	}
	if err := r.mayShareFromPrivateTeams(actor, before, l); err != nil {
		return l, err
	}
	if err := r.store.UpdateTeam(context.Background(), l); err != nil {
		return l, r.storeRefusal(l.Name, err)
	}
	r.requestSync()
	if l.Focus != before.Focus {
		r.setFocusTopic(l)
	}
	r.logChange(actor, "set team", l.Name, l.Name, changes(names, func(n string) (string, string) {
		return teamField(before, n), teamField(l, n)
	}))
	return l, nil
}

// teamField is one of a team's fields, as the log shows it.
func teamField(t org.Team, name string) string {
	switch name {
	case "LEADS":
		return strings.Join(t.Leads, ",")
	case "PARENT":
		return t.Parent
	case "FOCUS":
		return t.Focus
	case "REPOS":
		return strings.Join(t.Repos, ",")
	case "CONNECTORS":
		return strings.Join(t.Connectors, ",")
	case "CHATTER":
		return t.Chatter
	case "CHANNEL":
		return t.Channel
	case "BUDGET":
		return usd(t.MonthlyBudget) + "/month"
	case "PRIVATE":
		return strconv.FormatBool(t.Private)
	}
	return ""
}

// addMember adds a human to the org as actor.
func (r *Runner) addMember(actor, account string, fields []field) (org.Member, error) {
	m := org.Member{Account: account, Name: account, Role: "Team member"}
	if msg := r.checkNewNick(account); msg != "" {
		return m, refuse(invalid, "%s", msg)
	}
	explicit := false
	for _, f := range fields {
		if msg := r.applyMemberField(&m, f.name, f.value); msg != "" {
			return m, refuse(invalid, "%s", msg)
		}
		explicit = explicit || word(f.name) == "REPORTS-TO"
	}
	if err := r.allowed(actor, canInvite, where(m.Team), "add people"); err != nil {
		return m, err
	}
	if m.MonthlyCost != 0 {
		if err := r.allowed(actor, canBudget, where(m.Team), "set what people cost"); err != nil {
			return m, err
		}
	}
	newcomer := target{person: account, team: m.Team}
	if explicit {
		if err := r.mayMakeManager(actor, m.ReportsTo, newcomer); err != nil {
			return m, err
		}
	} else if m.ReportsTo = r.defaultManager(actor, m.Team, account); r.mayMakeManager(actor, m.ReportsTo, newcomer) != nil {
		m.ReportsTo = r.cfg.Owner // adding someone alone doesn't make actor their manager
	}
	if err := r.store.AddMember(context.Background(), m); err != nil {
		return m, r.storeRefusal(account, err)
	}
	r.letIn(m.Account)
	r.requestSync()
	r.logger.Printf("%s added member %s", actor, account)
	kind := "person"
	if m.Guest {
		kind = "guest"
	}
	r.logChange(actor, "add member", m.Account, m.Team, fmt.Sprintf("%s (%s), reporting to %s", m.Role, kind, m.ReportsTo))
	return m, nil
}

// setMember changes a human's place in the org as actor.
func (r *Runner) setMember(actor, account string, fields []field) (org.Member, error) {
	m, err := r.store.Member(context.Background(), account)
	if err != nil {
		return m, r.storeRefusal(account, err)
	}
	if err := r.allowed(actor, canArrange, onPerson(m.Account), "change people"); err != nil {
		return m, err
	}
	before := m
	var names []string
	for _, f := range fields {
		n := word(f.name)
		names = append(names, n)
		if msg := r.applyMemberField(&m, f.name, f.value); msg != "" {
			return m, refuse(invalid, "%s", msg)
		}
		switch n {
		case "TEAM":
			if err := r.allowed(actor, canArrange, where(m.Team), "move people"); err != nil {
				return m, err
			}
		case "COST":
			if err := r.allowed(actor, canBudget, onPerson(m.Account), "set what people cost"); err != nil {
				return m, err
			}
		case "GUEST":
			if err := r.allowed(actor, canInvite, onPerson(m.Account), "make people guests"); err != nil {
				return m, err
			}
			if m.Guest && len(r.teamsLedBy(m.Account)) > 0 {
				return m, refuse(invalid, "%s leads a team; guests can't, so DEMOTE them first.", m.Account)
			}
		}
	}
	if !strings.EqualFold(m.ReportsTo, before.ReportsTo) {
		if err := r.mayMakeManager(actor, m.ReportsTo, target{person: m.Account, team: m.Team}); err != nil {
			return m, err
		}
	}
	if err := r.store.UpdateMember(context.Background(), m); err != nil {
		return m, r.storeRefusal(account, err)
	}
	r.requestSync()
	r.logChange(actor, "set member", m.Account, m.Team, changes(names, func(n string) (string, string) {
		return memberField(before, n), memberField(m, n)
	}, "COST")) // what people cost stays private: just "changed"
	return m, nil
}

// memberField is one of a member's fields, as the log shows it.
func memberField(m org.Member, name string) string {
	switch name {
	case "NAME":
		return m.Name
	case "ROLE":
		return m.Role
	case "TEAM":
		return m.Team
	case "REPORTS-TO":
		return m.ReportsTo
	case "GUEST":
		return strconv.FormatBool(m.Guest)
	case "COST":
		return usd(m.MonthlyCost) // compared, never shown
	}
	return ""
}

// removeMember takes a human out of the org as actor.
func (r *Runner) removeMember(actor, account string) error {
	if r.isOwner(account) {
		return refuse(invalid, "the owner can't be removed.")
	}
	m, err := r.store.Member(context.Background(), account)
	if err != nil {
		return r.storeRefusal(account, err)
	}
	if err := r.allowed(actor, canInvite, onPerson(m.Account), "remove people"); err != nil {
		return err
	}
	if err := r.store.RemoveMember(context.Background(), account); err != nil {
		return r.storeRefusal(account, err)
	}
	r.lockOut(m.Account)
	r.requestSync()
	r.logger.Printf("%s removed member %s", actor, account)
	r.logChange(actor, "remove member", m.Account, m.Team, "")
	return nil
}

// promotePerson makes someone a lead of team (their own if it's ""), or,
// promote false, stops them leading it, as actor. movedTo is who a new lead
// now reports to, if their reporting line had to move out of the team.
func (r *Runner) promotePerson(actor, name, team string, promote bool) (l org.Team, movedTo string, err error) {
	ctx := context.Background()
	if !r.inOrg(name) {
		return l, "", r.storeRefusal(name, org.ErrNotFound)
	}
	name = r.properName(name)
	if team == "" {
		team = r.teamOf(name)
	}
	if team == "" {
		return l, "", refuse(invalid, "%s isn't in a team. Say which team, or set their TEAM first.", name)
	}
	if l, err = r.store.Team(ctx, team); err != nil {
		return l, "", r.storeRefusal(team, err)
	}
	if promote {
		if msg := r.checkLead(name); msg != "" {
			return l, "", refuse(invalid, "%s", msg)
		}
	}
	if err := r.mayLead(actor, inTeam(l.Name)); err != nil {
		return l, "", err
	}
	if !r.isOwner(actor) && (strings.EqualFold(name, actor) || r.below(actor, name)) {
		return l, "", refuse(forbidden, "you can't change whether you or anyone above you leads a team.")
	}
	switch {
	case promote && l.LedBy(name):
		return l, "", nil
	case promote:
		l.Leads = append(slices.Clone(l.Leads), name)
	case l.LedBy(name):
		l.Leads = slices.DeleteFunc(slices.Clone(l.Leads), func(x string) bool { return strings.EqualFold(x, name) })
	default:
		return l, "", refuse(invalid, "%s doesn't lead %s.", name, l.Name)
	}
	if err := r.store.UpdateTeam(ctx, l); err != nil {
		return l, "", r.storeRefusal(l.Name, err)
	}
	r.requestSync()
	action := "demote"
	if promote {
		action = "promote"
		r.logger.Printf("%s made %s lead of %s", actor, name, l.Name)
		movedTo = r.leadReportsOut(ctx, name, l)
	}
	r.logChange(actor, action, name, l.Name, "lead of "+l.Name)
	return l, movedTo, nil
}

// teamBudget is a team's budget as someone may see it: what people cost is
// private, so humans' costs are only shown to those who may see spend there.
type teamBudget struct {
	team        org.Team
	commitments []commitment
	committed   float64
	showCosts   bool // whether humans' costs may be shown to who's asking
}

func (r *Runner) budgetFor(actor, team string) (teamBudget, error) {
	l, err := r.store.Team(context.Background(), team)
	if err != nil {
		return teamBudget{}, r.storeRefusal(team, err)
	}
	if !r.seesTeam(actor, l.Name) {
		return teamBudget{}, refuse(forbidden, "%s is private: only the people in it see its budget.", l.Name)
	}
	cs := r.teamCommitments(l.Name, "")
	return teamBudget{team: l, commitments: cs, committed: total(cs), showCosts: r.can(actor, canSpend, inTeam(l.Name))}, nil
}
