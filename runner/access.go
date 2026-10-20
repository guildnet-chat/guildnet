// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Who may do what, and where. Companies organise themselves their own way,
// so this is made of pieces they put together: abilities, grouped into
// access levels, each working within a scope.
//
// The owner may do anything. Everyone else holds the levels that come with
// their place (Member for their own team, Lead for the teams they lead,
// Manager for everyone below them) and any they've been given (Admin, or
// levels the owner makes). The owner decides what each level holds. Guests
// hold nothing. And whatever anyone holds, three rails stay: nobody acts on
// themselves or anyone above them, nobody gives more than they hold, and
// only the owner changes the levels themselves or makes admins.

// ability is one thing a person may be allowed to do.
type ability string

const (
	canApprove ability = "approve" // decide agents' risky actions
	canHire    ability = "hire"    // hire, change, and fire agents
	canBudget  ability = "budgets" // agents' daily caps, and budgets of teams inside
	canInvite  ability = "invite"  // bring people in (and take them out), reset links
	canArrange ability = "arrange" // people's titles, teams, and managers; teams inside
	canSpend   ability = "spend"   // see spend, and what people cost
	canLog     ability = "log"     // see who changed what
)

var abilities = []ability{canApprove, canHire, canBudget, canInvite, canArrange, canSpend, canLog}

func isAbility(s string) bool { return slices.Contains(abilities, ability(s)) }

// Scopes: where a level's abilities work.
const (
	scopeOrg     = "org"     // everywhere
	scopeLed     = "led"     // the teams the holder leads
	scopeReports = "reports" // everyone below the holder
	scopeOwn     = "own"     // the holder's own team
	scopeTeams   = "teams"   // the teams the level names
)

var scopes = []string{scopeOrg, scopeLed, scopeReports, scopeOwn, scopeTeams}

// The preset levels. The owner may change what they hold, not where.
const (
	levelAdmin   = "Admin"
	levelLead    = "Lead"
	levelManager = "Manager"
	levelMember  = "Member"
)

func presets() []org.AccessLevel {
	all := make([]string, len(abilities))
	for i, a := range abilities {
		all[i] = string(a)
	}
	return []org.AccessLevel{
		{Name: levelAdmin, Abilities: all, Scope: scopeOrg},
		{Name: levelLead, Abilities: all, Scope: scopeLed},
		{Name: levelManager, Abilities: []string{string(canApprove), string(canSpend)}, Scope: scopeReports},
		{Name: levelMember, Scope: scopeOwn},
	}
}

func isPreset(name string) bool {
	return slices.ContainsFunc(presets(), func(l org.AccessLevel) bool { return strings.EqualFold(l.Name, name) })
}

// accessLevels are the presets as the owner has them, then the owner's own.
func (r *Runner) accessLevels() []org.AccessLevel {
	stored, _ := r.store.AccessLevels(context.Background())
	out := presets()
	for _, s := range stored {
		if i := slices.IndexFunc(out, func(l org.AccessLevel) bool { return strings.EqualFold(l.Name, s.Name) }); i >= 0 {
			out[i].Abilities = s.Abilities // a preset's scope never changes
			continue
		}
		out = append(out, s)
	}
	return out
}

func (r *Runner) accessLevel(name string) (org.AccessLevel, bool) {
	for _, l := range r.accessLevels() {
		if strings.EqualFold(l.Name, name) {
			return l, true
		}
	}
	return org.AccessLevel{}, false
}

// target is what an ability is used on: a person or agent (and so their
// team), a team, or, with neither, the org as a whole.
type target struct {
	person string
	team   string
}

func onPerson(name string) target { return target{person: name} }
func inTeam(team string) target   { return target{team: team} }

var wholeOrg = target{}

// describe says where a target is, for refusals.
func (t target) describe() string {
	switch {
	case t.person != "":
		return "for " + t.person
	case t.team != "":
		return "in " + t.team
	}
	return "for the whole org"
}

// held are the access levels someone holds: none for guests, agents, and
// strangers; Member, Lead, and Manager as their place gives them; and any
// they've been given.
func (r *Runner) held(account string) []org.AccessLevel {
	ctx := context.Background()
	m, err := r.store.Member(ctx, account)
	if err != nil || m.Guest {
		return nil
	}
	names := []string{levelMember}
	if len(r.teamsLedBy(account)) > 0 {
		names = append(names, levelLead)
	}
	if r.hasReports(account) {
		names = append(names, levelManager)
	}
	for _, a := range m.Access {
		if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, a) }) {
			names = append(names, a)
		}
	}
	var out []org.AccessLevel
	for _, n := range names {
		if l, ok := r.accessLevel(n); ok {
			out = append(out, l)
		}
	}
	return out
}

// holdsAbility reports whether actor holds ab anywhere at all.
func (r *Runner) holdsAbility(actor string, ab ability) bool {
	if r.isOwner(actor) {
		return true
	}
	for _, l := range r.held(actor) {
		if slices.Contains(l.Abilities, string(ab)) {
			return true
		}
	}
	return false
}

// can reports whether actor may use ab on t.
func (r *Runner) can(actor string, ab ability, t target) bool {
	if r.isOwner(actor) {
		return true
	}
	if actor == "" {
		return false
	}
	if t.person != "" {
		if strings.EqualFold(t.person, actor) || r.isOwner(t.person) || r.below(actor, t.person) {
			return false
		}
		if t.team == "" {
			t.team = r.teamOf(t.person)
		}
	}
	for _, l := range r.held(actor) {
		if slices.Contains(l.Abilities, string(ab)) && r.reaches(actor, l, t) {
			return true
		}
	}
	return false
}

// reaches reports whether level l, held by holder, reaches t.
func (r *Runner) reaches(holder string, l org.AccessLevel, t target) bool {
	switch l.Scope {
	case scopeOrg:
		return true
	case scopeReports:
		return t.person != "" && r.below(t.person, holder)
	case scopeLed:
		return r.within(t.team, r.teamsLedBy(holder))
	case scopeOwn:
		own := r.teamOf(holder)
		return own != "" && r.within(t.team, []string{own})
	case scopeTeams:
		return r.within(t.team, l.Teams)
	}
	return false
}

// below reports whether name reports, directly or not, to boss.
func (r *Runner) below(name, boss string) bool {
	cur := r.managerOf(name)
	for range 100 {
		if cur == "" {
			return false
		}
		if strings.EqualFold(cur, boss) {
			return true
		}
		cur = r.managerOf(cur)
	}
	return false
}

func (r *Runner) hasReports(name string) bool {
	for _, p := range r.people() {
		if strings.EqualFold(p.reportsTo, name) && !strings.EqualFold(p.name, name) {
			return true
		}
	}
	return false
}

// within reports whether team is one of roots or inside one of them.
func (r *Runner) within(team string, roots []string) bool {
	if team == "" {
		return false
	}
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[strings.ToLower(cur)]; cur = r.parentOf(cur) {
		seen[strings.ToLower(cur)] = true
		if slices.ContainsFunc(roots, func(x string) bool { return strings.EqualFold(x, cur) }) {
			return true
		}
	}
	return false
}

func (r *Runner) parentOf(team string) string {
	t, err := r.store.Team(context.Background(), team)
	if err != nil {
		return ""
	}
	return t.Parent
}

// teamsLedBy are the teams name leads.
func (r *Runner) teamsLedBy(name string) []string {
	teams, _ := r.store.Teams(context.Background())
	var out []string
	for _, t := range teams {
		if t.LedBy(name) {
			out = append(out, t.Name)
		}
	}
	return out
}

// allowed refuses unless actor may use ab on t; doing says what for, as in
// "hire", and the refusal reads "you can't hire in Sales".
func (r *Runner) allowed(actor string, ab ability, t target, doing string) error {
	if r.can(actor, ab, t) {
		return nil
	}
	return refuse(forbidden, "you can't %s %s.", doing, t.describe())
}

// holdsLevel reports whether actor holds everything level l gives, wherever
// it would reach for person: nobody gives more than they hold.
func (r *Runner) holdsLevel(actor string, l org.AccessLevel, person string) bool {
	if r.isOwner(actor) {
		return true
	}
	var reach []target
	switch l.Scope {
	case scopeOrg:
		reach = []target{wholeOrg}
	case scopeLed:
		for _, t := range r.teamsLedBy(person) {
			reach = append(reach, inTeam(t))
		}
	case scopeOwn:
		if t := r.teamOf(person); t != "" {
			reach = []target{inTeam(t)}
		}
	case scopeTeams:
		for _, t := range l.Teams {
			reach = append(reach, inTeam(t))
		}
	case scopeReports:
		for _, p := range r.people() {
			if r.below(p.name, person) {
				reach = append(reach, onPerson(p.name))
			}
		}
	}
	// Reaching nothing yet, it reaches whatever person is given later (a
	// team to lead, reports), by whoever gives that: only someone who holds
	// it across the org may give it.
	if len(reach) == 0 {
		reach = []target{wholeOrg}
	}
	for _, ab := range l.Abilities {
		for _, t := range reach {
			if !r.can(actor, ability(ab), t) {
				return false
			}
		}
	}
	return true
}

// mayLead refuses unless actor may choose who leads a team at t (the team,
// or for a new team, where it goes): they must be able to arrange there, and
// hold everything a lead would hold.
func (r *Runner) mayLead(actor string, t target) error {
	if err := r.allowed(actor, canArrange, t, "choose leads"); err != nil {
		return err
	}
	lead, _ := r.accessLevel(levelLead)
	for _, ab := range lead.Abilities {
		if !r.can(actor, ability(ab), t) {
			return refuse(forbidden, "you can't choose leads %s: a lead could %s there, and you can't.", t.describe(), ab)
		}
	}
	return nil
}

// mayMakeManager refuses unless actor may have subject (at t) report to
// manager. Everyone up manager's line then holds the Manager level over
// subject and whoever's below them (approve, say): any of them who doesn't
// already is given it by actor, and nobody gives more than they hold,
// themselves included.
func (r *Runner) mayMakeManager(actor, manager string, t target) error {
	l, _ := r.accessLevel(levelManager)
	for _, ab := range l.Abilities {
		if r.can(actor, ability(ab), t) {
			continue
		}
		seen := map[string]bool{}
		for cur := manager; cur != "" && !seen[strings.ToLower(cur)]; cur = r.managerOf(cur) {
			seen[strings.ToLower(cur)] = true
			if !r.can(cur, ability(ab), t) {
				return refuse(forbidden, "you can't have %s report to %s: %s would then %s %s, and you can't.", t.person, manager, cur, ab, t.describe())
			}
		}
	}
	return nil
}

// logChange records a change to the org.
func (r *Runner) logChange(actor, action, target, team, detail string) {
	e := org.LogEntry{At: r.now(), Actor: actor, Action: action, Target: target, Team: team, Detail: detail}
	if err := r.store.AddLog(context.Background(), e); err != nil {
		monitor.Problem(r.logger, "org log: %v", err)
	}
}

// logFor is the latest of the org log that actor may see.
func (r *Runner) logFor(actor string, limit int) []org.LogEntry {
	all, _ := r.store.Log(context.Background(), 1000)
	var out []org.LogEntry
	for _, e := range all {
		t := wholeOrg
		if e.Team != "" {
			t = inTeam(e.Team)
		}
		if r.can(actor, canLog, t) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

// Access levels: making, changing, giving, and taking.

// setAccessLevel makes or changes a level (the owner only).
func (r *Runner) setAccessLevel(actor, name string, abs []string, scope string, teams []string) (org.AccessLevel, error) {
	l := org.AccessLevel{Name: strings.TrimSpace(name), Abilities: abs, Scope: scope, Teams: teams}
	if !r.isOwner(actor) {
		return l, refuse(forbidden, "only %s (the owner) can change access levels.", r.cfg.Owner)
	}
	if l.Name == "" || len(l.Name) > 32 || strings.ContainsAny(l.Name, ",\r\n") {
		return l, refuse(invalid, "an access level's name is one line, at most 32 characters, without commas.")
	}
	for _, a := range abs {
		if !isAbility(a) {
			return l, refuse(invalid, "%q isn't an ability: use %s.", a, joinAbilities())
		}
	}
	if existing, ok := r.accessLevel(name); ok && isPreset(name) {
		l.Name, l.Scope, l.Teams = existing.Name, existing.Scope, nil // a preset's scope never changes
	} else if !slices.Contains(scopes, scope) {
		return l, refuse(invalid, "scope is one of %s.", strings.Join(scopes, ", "))
	}
	if l.Scope == scopeTeams {
		if len(teams) == 0 {
			return l, refuse(invalid, "say which teams it works in.")
		}
		for i, t := range teams {
			team, msg := r.checkTeam(t)
			if msg != "" {
				return l, refuse(invalid, "%s", msg)
			}
			l.Teams[i] = team
		}
	} else {
		l.Teams = nil
	}
	if err := r.store.SaveAccessLevel(context.Background(), l); err != nil {
		return l, r.storeRefusal(name, err)
	}
	r.logChange(actor, "set access", l.Name, "", describeLevel(l))
	return l, nil
}

// dropAccessLevel removes a level the owner made, and takes it from everyone.
func (r *Runner) dropAccessLevel(actor, name string) error {
	if !r.isOwner(actor) {
		return refuse(forbidden, "only %s (the owner) can change access levels.", r.cfg.Owner)
	}
	if isPreset(name) {
		return refuse(invalid, "%s is one of the presets; change what it holds instead.", name)
	}
	ctx := context.Background()
	if err := r.store.DropAccessLevel(ctx, name); err != nil {
		return r.storeRefusal(name, err)
	}
	members, _ := r.store.Members(ctx)
	for _, m := range members {
		if i := slices.IndexFunc(m.Access, func(a string) bool { return strings.EqualFold(a, name) }); i >= 0 {
			m.Access = slices.Delete(slices.Clone(m.Access), i, i+1)
			r.store.UpdateMember(ctx, m)
		}
	}
	r.logChange(actor, "drop access", name, "", "")
	return nil
}

// giveAccess gives person an access level (give true) or takes it away.
func (r *Runner) giveAccess(actor, person, name string, give bool) (org.Member, error) {
	ctx := context.Background()
	m, err := r.store.Member(ctx, person)
	if err != nil {
		if r.isAgent(person) {
			return m, refuse(invalid, "access is for people; agents act only through the people they work for.")
		}
		return m, r.storeRefusal(person, err)
	}
	l, ok := r.accessLevel(name)
	switch {
	case !ok:
		return m, refuse(notFound, "there's no access level %s. Try ACCESS to list them.", name)
	case strings.EqualFold(l.Name, levelLead) || strings.EqualFold(l.Name, levelManager) || strings.EqualFold(l.Name, levelMember):
		return m, refuse(invalid, "%s comes with someone's place: leading a team, having reports, or being in the org.", l.Name)
	case give && m.Guest:
		return m, refuse(invalid, "%s is a guest, and guests can't be given access.", m.Account)
	case r.isOwner(m.Account):
		return m, refuse(invalid, "the owner already has every ability.")
	case strings.EqualFold(l.Name, levelAdmin) && !r.isOwner(actor):
		return m, refuse(forbidden, "only %s (the owner) can make admins.", r.cfg.Owner)
	case !r.isOwner(actor) && (strings.EqualFold(actor, m.Account) || r.below(actor, m.Account)):
		return m, refuse(forbidden, "you can't change the access of yourself or anyone above you.")
	case !r.can(actor, canArrange, onPerson(m.Account)):
		return m, refuse(forbidden, "you can't change %s's access: they're outside the people you arrange.", m.Account)
	case !r.holdsLevel(actor, l, m.Account):
		return m, refuse(forbidden, "you can't give or take %s: it reaches further than you do.", l.Name)
	}
	has := slices.ContainsFunc(m.Access, func(a string) bool { return strings.EqualFold(a, l.Name) })
	switch {
	case give && !has:
		m.Access = append(slices.Clone(m.Access), l.Name)
	case !give && has:
		m.Access = slices.DeleteFunc(slices.Clone(m.Access), func(a string) bool { return strings.EqualFold(a, l.Name) })
	default:
		return m, nil
	}
	if err := r.store.UpdateMember(ctx, m); err != nil {
		return m, r.storeRefusal(m.Account, err)
	}
	action := "give access"
	if !give {
		action = "take access"
	}
	r.logChange(actor, action, m.Account, m.Team, l.Name)
	return m, nil
}

func joinAbilities() string {
	names := make([]string, len(abilities))
	for i, a := range abilities {
		names[i] = string(a)
	}
	return strings.Join(names, ", ")
}

// describeLevel is a level in words: "Ops: spend, log, across the org".
func describeLevel(l org.AccessLevel) string {
	what := strings.Join(l.Abilities, ", ")
	if what == "" {
		what = "nothing extra"
	}
	where := map[string]string{
		scopeOrg:     "across the org",
		scopeLed:     "in the teams they lead",
		scopeReports: "for everyone below them",
		scopeOwn:     "in their own team",
		scopeTeams:   "in " + strings.Join(l.Teams, ", "),
	}[l.Scope]
	return fmt.Sprintf("%s: %s, %s", l.Name, what, where)
}

// GuildServ's ACCESS and LOG.

func (r *Runner) guildAccess(from, actor string, args []string) {
	usage := `usage: ACCESS, ACCESS GIVE|TAKE <person> <level>, ACCESS SET <level> <abilities,...|none> [SCOPE org|led|reports|own|teams] [TEAMS a,b], or ACCESS DROP <level>. Quote names with spaces: "Sales helper".`
	if len(args) == 0 || strings.EqualFold(args[0], "LIST") {
		for _, l := range r.accessLevels() {
			r.guildReply(from, describeLevel(l))
		}
		var mine []string
		for _, l := range r.held(actor) {
			mine = append(mine, l.Name)
		}
		if r.isOwner(actor) {
			mine = []string{"Owner (everything)"}
		}
		r.guildReply(from, "You: "+strings.Join(mine, ", ")+". Abilities: "+joinAbilities()+".")
		return
	}
	switch strings.ToUpper(args[0]) {
	case "GIVE", "TAKE":
		if len(args) < 3 {
			r.guildReply(from, usage)
			return
		}
		give := strings.EqualFold(args[0], "GIVE")
		m, err := r.giveAccess(actor, args[1], strings.Join(args[2:], " "), give)
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		has := "no access given"
		if len(m.Access) > 0 {
			has = strings.Join(m.Access, ", ")
		}
		r.guildReply(from, fmt.Sprintf("%s now has: %s.", m.Account, has))
	case "SET":
		if len(args) < 3 {
			r.guildReply(from, usage)
			return
		}
		var abs []string
		if !strings.EqualFold(args[2], "none") {
			for _, a := range strings.Split(args[2], ",") {
				if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
					abs = append(abs, a)
				}
			}
		}
		scope, teams := scopeTeams, []string(nil)
		if existing, ok := r.accessLevel(args[1]); ok {
			scope, teams = existing.Scope, existing.Teams
		}
		for rest := args[3:]; len(rest) > 0; rest = rest[2:] {
			if len(rest) < 2 {
				r.guildReply(from, usage)
				return
			}
			switch strings.ToUpper(rest[0]) {
			case "SCOPE":
				scope = strings.ToLower(rest[1])
			case "TEAMS":
				teams = nil
				for _, x := range strings.Split(rest[1], ",") {
					if x = strings.TrimSpace(x); x != "" {
						teams = append(teams, x)
					}
				}
			default:
				r.guildReply(from, usage)
				return
			}
		}
		l, err := r.setAccessLevel(actor, args[1], abs, scope, teams)
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, "access level "+describeLevel(l)+".")
	case "DROP":
		if len(args) < 2 {
			r.guildReply(from, usage)
			return
		}
		name := strings.Join(args[1:], " ")
		if err := r.dropAccessLevel(actor, name); err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, "dropped the access level "+name+", and took it from everyone who had it.")
	default:
		r.guildReply(from, usage)
	}
}

func (r *Runner) guildLog(from, actor string, args []string) {
	n := 10
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = min(v, 30)
		}
	}
	entries := r.logFor(actor, n)
	if len(entries) == 0 {
		r.guildReply(from, "nothing in the log that you may see.")
		return
	}
	for i := len(entries) - 1; i >= 0; i-- { // oldest first, as chat reads
		e := entries[i]
		line := fmt.Sprintf("%s %s %s %s", e.At.In(r.loc()).Format("Jan 2 15:04"), e.Actor, e.Action, e.Target)
		if e.Team != "" && !strings.EqualFold(e.Team, e.Target) {
			line += " (" + e.Team + ")"
		}
		if e.Detail != "" {
			line += ": " + e.Detail
		}
		r.guildReply(from, truncate(line, maxLineBytes))
	}
}
