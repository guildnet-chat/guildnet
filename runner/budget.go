// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/org"
)

// commitment is one line on a team's budget: a human's monthly cost, an
// agent's daily cap for a month, or the budget of a team inside it.
type commitment struct {
	name    string
	monthly float64
	agent   bool
	team    bool
}

// teamCommitments lists what's committed against team's budget, leaving out
// the agent or inner team named except (so a changed one isn't counted
// twice).
func (r *Runner) teamCommitments(team, except string) []commitment {
	var out []commitment
	teams, _ := r.store.Teams(context.Background())
	for _, t := range teams {
		if strings.EqualFold(t.Parent, team) && !strings.EqualFold(t.Name, except) && t.MonthlyBudget > 0 {
			out = append(out, commitment{name: t.Name, monthly: t.MonthlyBudget, team: true})
		}
	}
	members, _ := r.store.Members(context.Background())
	for _, m := range members {
		if strings.EqualFold(m.Team, team) && m.MonthlyCost > 0 {
			out = append(out, commitment{name: m.Account, monthly: m.MonthlyCost})
		}
	}
	for _, p := range r.staff() {
		// Outside agents too: their daily budget is what they may spend (through
		// the model gateway, where there's one).
		if strings.EqualFold(p.Team, team) && !strings.EqualFold(p.Nick, except) {
			out = append(out, commitment{name: p.Nick, monthly: org.AgentMonthlyCost(p.DailyBudgetUSD), agent: true})
		}
	}
	return out
}

// spendNote is what an agent may spend, in words, for the log: an agent
// outside GuildNet spends through the model gateway, where the network has
// one, and otherwise on keys of its own.
func (r *Runner) spendNote(p config.Persona) string {
	if p.Platform == PlatformOutside {
		via := ""
		if r.modelCalls() != nil {
			via = " through the model gateway"
		}
		return fmt.Sprintf("outside GuildNet, %s/day%s, %d wake-ups a day", usd(p.DailyBudgetUSD), via, wakeCap(p))
	}
	return usd(p.DailyBudgetUSD) + "/day"
}

func total(cs []commitment) float64 {
	sum := 0.0
	for _, c := range cs {
		sum += c.monthly
	}
	return sum
}

// checkTeamBudget decides whether agent nick, with a daily cap of dailyCap,
// fits in team. Leads are refused when it doesn't fit; the owner may go over
// but is warned. It returns a refusal or a warning, at most one of them.
func (r *Runner) checkTeamBudget(actor, team, nick string, dailyCap float64) (refusal, warning string) {
	if team == "" {
		return "", ""
	}
	l, err := r.store.Team(context.Background(), team)
	if err != nil {
		return "", ""
	}
	cost := org.AgentMonthlyCost(dailyCap)
	left := l.MonthlyBudget - total(r.teamCommitments(l.Name, nick))

	if r.isOwner(actor) {
		if l.MonthlyBudget > 0 && cost > left {
			return "", fmt.Sprintf("note: %s is now over its %s/month budget by %s.", l.Name, usd(l.MonthlyBudget), usd(cost-left))
		}
		return "", ""
	}
	switch {
	case l.MonthlyBudget <= 0:
		return fmt.Sprintf("%s has no budget yet, so only %s (the owner) can add spend there. The owner can TEAM SET %s BUDGET <usd per month>.",
			l.Name, r.cfg.Owner, l.Name), ""
	case dailyCap <= 0:
		return fmt.Sprintf("agents in %s need a daily BUDGET so they fit the team's budget.", l.Name), ""
	case cost > left:
		return fmt.Sprintf("%s would cost %s/month, but %s has only %s left of its %s/month budget.",
			nick, usd(cost), l.Name, usd(max(left, 0)), usd(l.MonthlyBudget)), ""
	}
	return "", ""
}

// checkTeamBudgetChange decides whether t's (new) budget may be set by
// actor: a team at the top of the org gets its budget from the owner; one
// inside another is carved out of that team's budget by someone who may set
// budgets there, and must fit what's left of it. The owner may go over, and
// is warned.
func (r *Runner) checkTeamBudgetChange(actor string, t org.Team) (warning string, err error) {
	if t.Parent == "" {
		if !r.isOwner(actor) {
			return "", refuse(forbidden, "only %s (the owner) sets the budgets of teams at the top of the org.", r.cfg.Owner)
		}
		return "", nil
	}
	if err := r.allowed(actor, canBudget, inTeam(t.Parent), "set budgets"); err != nil {
		return "", err
	}
	parent, err := r.store.Team(context.Background(), t.Parent)
	if err != nil {
		return "", r.storeRefusal(t.Parent, err)
	}
	left := parent.MonthlyBudget - total(r.teamCommitments(parent.Name, t.Name))
	if t.MonthlyBudget <= left {
		return "", nil
	}
	if r.isOwner(actor) {
		return fmt.Sprintf("note: %s is now over its %s/month budget by %s.", parent.Name, usd(parent.MonthlyBudget), usd(t.MonthlyBudget-left)), nil
	}
	return "", refuse(forbidden, "%s/month for %s doesn't fit: %s has only %s left of its %s/month budget.",
		usd(t.MonthlyBudget), t.Name, parent.Name, usd(max(left, 0)), usd(parent.MonthlyBudget))
}

// guildBudget runs BUDGET [team]: what each team has, what's committed, and
// what's left. Humans' costs are shown only to the owner and their lead.
func (r *Runner) guildBudget(from, actor string, args []string) {
	var names []string
	if len(args) > 0 {
		names = []string{strings.Join(args, " ")}
	} else {
		teams, _ := r.store.Teams(context.Background())
		for _, l := range teams {
			names = append(names, l.Name)
		}
	}
	for _, name := range names {
		b, err := r.budgetFor(actor, name)
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, describeBudget(b))
	}
}

func describeBudget(b teamBudget) string {
	var humans, agents, inner []string
	for _, c := range b.commitments {
		switch {
		case c.agent:
			agents = append(agents, c.name+" "+usd(c.monthly))
		case c.team:
			inner = append(inner, c.name+" "+usd(c.monthly))
		case b.showCosts:
			humans = append(humans, c.name+" "+usd(c.monthly))
		default:
			humans = append(humans, c.name)
		}
	}
	list := func(items []string) string {
		if len(items) == 0 {
			return "none"
		}
		return strings.Join(items, ", ")
	}
	l := b.team
	s := fmt.Sprintf("%s: ", l.Name)
	if l.MonthlyBudget > 0 {
		s += usd(l.MonthlyBudget) + "/month. "
	} else {
		s += "no budget set. "
	}
	s += fmt.Sprintf("Humans: %s. Agents: %s.", list(humans), list(agents))
	if len(inner) > 0 {
		s += fmt.Sprintf(" Teams inside: %s.", list(inner))
	}
	if !b.showCosts {
		return s // totals would give people's costs away
	}
	s = strings.TrimSuffix(s, ".") + fmt.Sprintf(". %s committed", usd(b.committed))
	if l.MonthlyBudget > 0 {
		if left := l.MonthlyBudget - b.committed; left >= 0 {
			s += ", " + usd(left) + " left"
		} else {
			s += ", over by " + usd(-left)
		}
	}
	return s + "."
}
func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }
