// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// isOwner reports whether account owns the network.
func (r *Runner) isOwner(account string) bool {
	return account != "" && strings.EqualFold(account, r.cfg.Owner)
}

// where is the target for something in team: the team, or the whole org
// for something in no team.
func where(team string) target {
	if team == "" {
		return wholeOrg
	}
	return inTeam(team)
}

// defaultManager is who a newcomer reports to when nobody says: whoever's
// adding them if they lead the team (or one it's inside), else the nearest
// lead of the team or a team it's inside, else whoever's adding them, else
// the owner.
func (r *Runner) defaultManager(actor, team, newcomer string) string {
	if !r.isOwner(actor) && r.within(team, r.teamsLedBy(actor)) {
		return actor
	}
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[strings.ToLower(cur)]; cur = r.parentOf(cur) {
		seen[strings.ToLower(cur)] = true
		if t, err := r.store.Team(context.Background(), cur); err == nil {
			for _, l := range t.Leads {
				if !strings.EqualFold(l, newcomer) {
					return l
				}
			}
		}
	}
	if !r.isOwner(actor) && actor != "" {
		return actor
	}
	return r.cfg.Owner
}

// checkTeam returns the team's proper name, or a problem to report.
func (r *Runner) checkTeam(name string) (string, string) {
	l, err := r.store.Team(context.Background(), name)
	if err != nil {
		return "", fmt.Sprintf("there's no team %s. TEAM CREATE %s first.", name, name)
	}
	return l.Name, ""
}

// inOrg reports whether name is the owner, a member, or an agent.
func (r *Runner) inOrg(name string) bool {
	ctx := context.Background()
	if r.isOwner(name) || r.isAgent(name) {
		return true
	}
	_, err := r.store.Member(ctx, name)
	return err == nil
}

// teamLabel describes someone's place in their team: "Sales team (lead)".
func (r *Runner) teamLabel(name, team string) string {
	if team == "" {
		return "no team"
	}
	label := team + " team"
	if l, err := r.store.Team(context.Background(), team); err == nil && l.LedBy(name) {
		label += " (lead)"
	}
	return label
}

func (r *Runner) guildTeams(from string) {
	teams, _ := r.store.Teams(context.Background())
	if len(teams) == 0 {
		r.guildReply(from, "no teams yet. TEAM CREATE <name> makes one.")
		return
	}
	for _, l := range teams {
		r.guildReply(from, describeTeam(l))
	}
}

func describeTeam(l org.Team) string {
	s := l.Name + " (" + l.Channel
	if l.Private {
		s += ", private"
	}
	s += ")"
	if len(l.Leads) > 0 {
		s += " led by " + andList(l.Leads)
	}
	if l.Parent != "" {
		s += ", inside " + l.Parent
	}
	if len(l.Repos) > 0 {
		s += ", repos " + strings.Join(l.Repos, ", ")
	}
	if len(l.Connectors) > 0 {
		s += ", connectors " + strings.Join(l.Connectors, ", ")
	}
	if l.Chatter != "" {
		s += ", chatter " + l.Chatter
	}
	if l.MonthlyBudget > 0 {
		s += ", budget " + usd(l.MonthlyBudget) + "/month"
	}
	return s
}

// andList is "a", "a and b", or "a, b and c".
func andList(xs []string) string {
	if len(xs) <= 1 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// parseUSD reads an amount like 2000, 2,000, or $2000.50.
func parseUSD(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(s, "$"), ",", ""), 64)
	return v, err == nil && v >= 0
}

// guildTeam runs TEAM CREATE and TEAM SET.
func (r *Runner) guildTeam(from, actor string, args []string) {
	usage := "usage: TEAM CREATE <name> [PARENT team] [LEAD name] [FOCUS \"...\"] [CHANNEL #x] [BUDGET usd/month] [PRIVATE], or TEAM SET <name> LEADS|PARENT|FOCUS|REPOS|CONNECTORS|CHATTER|CHANNEL|BUDGET|PRIVATE <value>."
	if len(args) < 2 {
		r.guildReply(from, usage)
		return
	}
	switch strings.ToUpper(args[0]) {
	case "CREATE":
		var fields []field
		for rest := args[2:]; len(rest) > 0; {
			switch word(rest[0]) {
			case "PRIVATE":
				fields, rest = append(fields, field{"PRIVATE", "on"}), rest[1:]
			case "LEAD", "LEADS", "PARENT", "FOCUS", "REPOS", "CONNECTORS", "CHATTER", "CHANNEL", "BUDGET":
				if len(rest) < 2 {
					r.guildReply(from, usage)
					return
				}
				fields, rest = append(fields, field{rest[0], rest[1]}), rest[2:]
			default:
				r.guildReply(from, usage)
				return
			}
		}
		l, err := r.createTeam(actor, args[1], fields)
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, "created team "+describeTeam(l)+".")
	case "SET":
		if len(args) < 4 {
			r.guildReply(from, usage)
			return
		}
		l, err := r.setTeam(actor, args[1], []field{{args[2], strings.Join(args[3:], " ")}})
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, "updated team "+describeTeam(l)+".")
	default:
		r.guildReply(from, usage)
	}
}
func (r *Runner) applyTeamField(l *org.Team, field, value string) string {
	switch word(field) {
	case "LEAD", "LEADS":
		var leads []string
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" || strings.EqualFold(name, "none") {
				continue
			}
			if msg := r.checkLead(name); msg != "" {
				return msg
			}
			leads = append(leads, r.properName(name))
		}
		l.Leads = leads
	case "PARENT":
		if value == "" || strings.EqualFold(value, "none") {
			l.Parent = ""
			break
		}
		parent, msg := r.checkTeam(value)
		if msg != "" {
			return msg
		}
		if r.within(parent, []string{l.Name}) {
			return fmt.Sprintf("%s can't go inside %s: %s is inside %s already.", l.Name, parent, parent, l.Name)
		}
		l.Parent = parent
	case "REPOS":
		if value == "" || strings.EqualFold(value, "none") {
			l.Repos = nil
			break
		}
		available := r.githubRepos()
		var repos []string
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if rest, ok := strings.CutPrefix(strings.ToLower(name), gitPrefix); ok {
				repo, msg := r.gitRepo(rest)
				if msg != "" {
					return msg
				}
				if !slices.Contains(repos, gitPrefix+repo.Name) {
					repos = append(repos, gitPrefix+repo.Name)
				}
				continue
			}
			if len(available) == 0 {
				return "GitHub isn't connected to this network yet: the owner connects it in the console's Settings."
			}
			i := slices.IndexFunc(available, func(a string) bool { return strings.EqualFold(a, name) })
			if i < 0 {
				return fmt.Sprintf("%s isn't one of the repos GitHub gives this network: %s.", name, strings.Join(available, ", "))
			}
			if !slices.Contains(repos, available[i]) {
				repos = append(repos, available[i])
			}
		}
		l.Repos = repos
	case "CHATTER":
		switch v := strings.ToLower(strings.TrimSpace(value)); v {
		case chatterQuiet, chatterSummary, chatterSteps:
			l.Chatter = v
		case "", "none", "default":
			l.Chatter = ""
		default:
			return "CHATTER is quiet, summary or steps: how much of its agents' work the team's channel sees."
		}
	case "CONNECTORS":
		ids, msg := r.teamConnectors(value)
		if msg != "" {
			return msg
		}
		l.Connectors = ids
	case "FOCUS":
		if len(value) > maxGoalText || strings.ContainsAny(value, "\r\n") {
			return fmt.Sprintf("FOCUS is one line, at most %d characters.", maxGoalText)
		}
		l.Focus = strings.TrimSpace(value)
	case "CHANNEL":
		if msg := r.checkTeamChannel(l.Name, value); msg != "" {
			return msg
		}
		l.Channel = value
	case "BUDGET":
		v, ok := parseUSD(value)
		if !ok {
			return "BUDGET is dollars per month, like 2000."
		}
		l.MonthlyBudget = v
	case "PRIVATE":
		switch strings.ToLower(value) {
		case "on", "yes", "true":
			l.Private = true
		case "off", "no", "false":
			l.Private = false
		default:
			return "PRIVATE is on or off."
		}
	default:
		return fmt.Sprintf("unknown team field %s: use LEADS, PARENT, FOCUS, REPOS, CONNECTORS, CHATTER, CHANNEL, BUDGET, or PRIVATE.", strings.ToUpper(field))
	}
	return ""
}

// guildMember runs MEMBER ADD, SET, and REMOVE for humans.
func (r *Runner) guildMember(from, actor string, args []string) {
	usage := `usage: MEMBER ADD <account> [ROLE "..."] [NAME "..."] [TEAM ...] [REPORTS-TO name] [GUEST], MEMBER SET <account> <field> <value>, or MEMBER REMOVE <account>.`
	if len(args) < 2 {
		r.guildReply(from, usage)
		return
	}
	account := args[1]
	switch strings.ToUpper(args[0]) {
	case "ADD":
		var fields []field
		for rest := args[2:]; len(rest) > 0; {
			if strings.EqualFold(rest[0], "GUEST") {
				fields, rest = append(fields, field{"GUEST", "on"}), rest[1:]
				continue
			}
			if len(rest) < 2 {
				r.guildReply(from, usage)
				return
			}
			fields, rest = append(fields, field{rest[0], rest[1]}), rest[2:]
		}
		m, err := r.addMember(actor, account, fields)
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, fmt.Sprintf("added %s (%s), %s, reporting to %s.", account, m.Role, r.teamLabel(account, m.Team), m.ReportsTo))
	case "SET":
		if len(args) < 4 {
			r.guildReply(from, usage)
			return
		}
		m, err := r.setMember(actor, account, []field{{args[2], strings.Join(args[3:], " ")}})
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, fmt.Sprintf("updated %s: %s, %s, reporting to %s.", account, m.Role, r.teamLabel(account, m.Team), m.ReportsTo))
	case "REMOVE":
		if err := r.removeMember(actor, account); err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, fmt.Sprintf("removed %s from the org.", account))
	default:
		r.guildReply(from, usage)
	}
}
func (r *Runner) applyMemberField(m *org.Member, field, value string) string {
	switch word(field) {
	case "NAME":
		m.Name = value
	case "ROLE":
		m.Role = value
	case "TEAM":
		l, msg := r.checkTeam(value)
		if msg != "" {
			return msg
		}
		m.Team = l
	case "REPORTS-TO":
		if !r.inOrg(value) {
			return fmt.Sprintf("%s isn't in the org.", value)
		}
		if r.reportsLoop(m.Account, value) {
			return fmt.Sprintf("%s can't report to %s: that would make a reporting loop.", m.Account, value)
		}
		m.ReportsTo = value
	case "GUEST":
		m.Guest = strings.EqualFold(value, "on") || strings.EqualFold(value, "yes") || strings.EqualFold(value, "true")
	case "COST":
		v, ok := parseUSD(value)
		if !ok {
			return "COST is dollars per month, like 6000."
		}
		m.MonthlyCost = v
	default:
		return fmt.Sprintf("unknown member field %s: use NAME, ROLE, TEAM, REPORTS-TO, COST, or GUEST.", strings.ToUpper(field))
	}
	return ""
}

// guildPromote makes someone a lead of their team (or the team named), or
// (promote false) stops them leading it.
func (r *Runner) guildPromote(from, actor string, args []string, promote bool) {
	if len(args) < 1 {
		r.guildReply(from, "usage: PROMOTE <name> [team] or DEMOTE <name> [team]")
		return
	}
	name, team := args[0], strings.Join(args[1:], " ")
	l, movedTo, err := r.promotePerson(actor, name, team, promote)
	if err != nil {
		r.guildReply(from, err.Error())
		return
	}
	name = r.properName(name)
	if !promote {
		r.guildReply(from, fmt.Sprintf("%s no longer leads %s.", name, l.Name))
		return
	}
	reply := fmt.Sprintf("%s is now lead of %s.", name, l.Name)
	if movedTo != "" {
		reply += fmt.Sprintf(" %s now reports to %s.", name, movedTo)
	}
	r.guildReply(from, reply)
}

// checkLead says what's wrong with name leading a team, if anything.
func (r *Runner) checkLead(name string) string {
	if !r.inOrg(name) {
		return fmt.Sprintf("%s isn't in the org.", name)
	}
	if m, err := r.store.Member(context.Background(), name); err == nil && m.Guest {
		return fmt.Sprintf("%s is a guest, and guests can't lead a team.", m.Account)
	}
	return ""
}

// properName is name as the org spells it.
func (r *Runner) properName(name string) string {
	ctx := context.Background()
	if p, err := r.store.Agent(ctx, name); err == nil {
		return p.Nick
	}
	if m, err := r.store.Member(ctx, name); err == nil {
		return m.Account
	}
	return name
}

// person is anyone in the org chart, human or agent, for display.
type person struct {
	name, role, team, reportsTo string
	agent, guest                bool
	access                      []string // levels they've been given
}

func (r *Runner) people() []person {
	ctx := context.Background()
	var out []person
	members, _ := r.store.Members(ctx)
	for _, m := range members {
		out = append(out, person{name: m.Account, role: m.Role, team: m.Team, reportsTo: m.ReportsTo, guest: m.Guest, access: m.Access})
	}
	for _, p := range r.staff() {
		out = append(out, person{name: p.Nick, role: p.Role, team: p.Team, reportsTo: p.ReportsTo, agent: true})
	}
	return out
}

func (r *Runner) describePerson(p person) string {
	kind := "human"
	switch {
	case p.agent:
		kind = "AI agent"
	case p.guest:
		kind = "guest"
	}
	if r.isOwner(p.name) {
		return fmt.Sprintf("%s (%s): %s, owner", p.name, kind, p.role)
	}
	return fmt.Sprintf("%s (%s): %s, %s", p.name, kind, p.role, r.teamLabel(p.name, p.team))
}

// guildOrg shows the reporting tree, owner first, reports indented under
// their manager.
func (r *Runner) guildOrg(from string) {
	people := r.people()
	byName := map[string]person{}
	reports := map[string][]person{}
	for _, p := range people {
		byName[strings.ToLower(p.name)] = p
	}
	var roots []person
	for _, p := range people {
		mgr := strings.ToLower(p.reportsTo)
		if _, ok := byName[mgr]; ok && !strings.EqualFold(p.name, p.reportsTo) && !r.isOwner(p.name) {
			reports[mgr] = append(reports[mgr], p)
		} else {
			roots = append(roots, p)
		}
	}
	sortPeople := func(ps []person) {
		sort.Slice(ps, func(i, j int) bool {
			if r.isOwner(ps[i].name) != r.isOwner(ps[j].name) {
				return r.isOwner(ps[i].name)
			}
			return strings.ToLower(ps[i].name) < strings.ToLower(ps[j].name)
		})
	}
	sortPeople(roots)
	seen := map[string]bool{}
	var lines []string
	var walk func(p person, depth int)
	walk = func(p person, depth int) {
		k := strings.ToLower(p.name)
		if seen[k] || len(lines) >= 40 {
			return
		}
		seen[k] = true
		lines = append(lines, strings.Repeat("  ", depth)+r.describePerson(p))
		kids := reports[k]
		sortPeople(kids)
		for _, c := range kids {
			walk(c, depth+1)
		}
	}
	for _, p := range roots {
		walk(p, 0)
	}
	for _, l := range lines {
		r.guildReply(from, l)
	}
}

// teamSummary is the org chart as agents see it when they wake.
func (r *Runner) teamSummary() string {
	var s strings.Builder
	for _, p := range r.people() {
		s.WriteString("- " + r.describePerson(p))
		if p.reportsTo != "" && !r.isOwner(p.name) {
			s.WriteString(", reports to " + p.reportsTo)
		}
		s.WriteByte('\n')
	}
	teams, _ := r.store.Teams(context.Background())
	if len(teams) > 0 {
		s.WriteString("Teams: ")
		parts := make([]string, 0, len(teams))
		for _, l := range teams {
			parts = append(parts, describeTeam(l))
		}
		s.WriteString(strings.Join(parts, "; "))
		s.WriteByte('\n')
	}
	return s.String()
}

// managerOf is who someone reports to: "" for the owner, or anyone unknown.
func (r *Runner) managerOf(name string) string {
	ctx := context.Background()
	if p, err := r.store.Agent(ctx, name); err == nil {
		return p.ReportsTo
	}
	if m, err := r.store.Member(ctx, name); err == nil {
		return m.ReportsTo
	}
	return ""
}

// reportsLoop says whether name reporting to manager would make a loop:
// manager already reports, somewhere up the line, to name.
func (r *Runner) reportsLoop(name, manager string) bool {
	cur := manager
	for range 100 {
		if strings.EqualFold(cur, name) {
			return true
		}
		if cur = r.managerOf(cur); cur == "" {
			return false
		}
	}
	return true
}

// leadReportsOut moves a new lead's reporting line to the owner if it was
// to someone they now lead: someone in their team, or a team inside it, who
// isn't a lead of it too. It returns who they report to now, or "" if
// nothing changed.
func (r *Runner) leadReportsOut(ctx context.Context, name string, team org.Team) string {
	manager := r.managerOf(name)
	if manager == "" || team.LedBy(manager) || !r.within(r.teamOf(manager), []string{team.Name}) {
		return ""
	}
	if p, err := r.store.Agent(ctx, name); err == nil {
		p.ReportsTo = r.cfg.Owner
		if err := r.store.Update(ctx, p); err != nil {
			return ""
		}
		r.updateAgent(p)
	} else if m, err := r.store.Member(ctx, name); err == nil {
		m.ReportsTo = r.cfg.Owner
		if err := r.store.UpdateMember(ctx, m); err != nil {
			return ""
		}
	}
	return r.cfg.Owner
}

// teamOf is someone's team.
func (r *Runner) teamOf(name string) string {
	ctx := context.Background()
	if p, err := r.store.Agent(ctx, name); err == nil {
		return p.Team
	}
	if m, err := r.store.Member(ctx, name); err == nil {
		return m.Team
	}
	return ""
}

// githubRepos are the repos GitHub gives this network, for teams to choose
// from; none when GitHub isn't set up.
func (r *Runner) githubRepos() []string {
	_, repos := r.githubInstallation()
	return repos
}

// githubInstallation is the account and repos GitHub gives this network.
func (r *Runner) githubInstallation() (account string, repos []string) {
	if r.cfg.GitHubLive != nil {
		return r.cfg.GitHubLive()
	}
	return r.cfg.GitHub.Owner, r.cfg.GitHub.Repos
}

// ReposFor is the repos a team works on: its own, or those of the nearest
// team it's inside that has some.
func (r *Runner) ReposFor(team string) []string {
	return org.ReposFor(context.Background(), r.store, team)
}

// channelsRepos is the repos (account/repo) of the teams whose channels are
// among channels, by channel, for linking "PR #42" there to the right repo.
// Only repos the installation has count, as for agents.
func (r *Runner) channelsRepos(channels []string) map[string][]string {
	out := map[string][]string{}
	account, have := r.githubInstallation()
	if account == "" {
		return out
	}
	teams, _ := r.store.Teams(context.Background())
	for _, t := range teams {
		i := slices.IndexFunc(channels, func(c string) bool { return strings.EqualFold(c, t.Channel) })
		if t.Channel == "" || i < 0 {
			continue
		}
		for _, repo := range r.ReposFor(t.Name) {
			if slices.Contains(have, repo) {
				out[channels[i]] = append(out[channels[i]], account+"/"+repo)
			}
		}
	}
	return out
}

// importedTeamRepos marks that guildnet.yaml's github.teams have been
// carried into the org, which counts from then on.
const importedTeamRepos = "github_teams_imported"

// importTeamRepos carries the repos guildnet.yaml gave each team (from
// before teams kept their own) into the org, once.
func (r *Runner) importTeamRepos() {
	ctx := context.Background()
	if len(r.cfg.GitHub.Teams) == 0 {
		return
	}
	if done, _ := r.store.Setting(ctx, importedTeamRepos); done != "" {
		return
	}
	for name, repos := range r.cfg.GitHub.Teams {
		t, err := r.store.Team(ctx, name)
		if err != nil || len(t.Repos) > 0 {
			continue
		}
		t.Repos = repos
		if err := r.store.UpdateTeam(ctx, t); err != nil {
			monitor.Problem(r.logger, "carrying %s's repos into the org: %v", name, err)
			return
		}
		r.logger.Printf("%s works on %s (from guildnet.yaml)", t.Name, strings.Join(repos, ", "))
	}
	r.store.SetSetting(ctx, importedTeamRepos, "yes")
}
