// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Managing the org from the console: people's places and access, teams,
// the company's description and goals, access levels, and the org's log.
// Everything GuildServ can do, in forms. The console offers what the runner
// says the signed-in person may do (GET /v1/me); the runner checks every
// change again, so nothing here decides who may do what.

// orgFor loads the org chart and what the signed-in person may do.
func (p *Platform) orgFor(r *http.Request) (orgChart, me, error) {
	var chart orgChart
	if err := p.runner(r).call(r.Context(), "GET", "/v1/org", nil, &chart); err != nil {
		return chart, me{}, err
	}
	m, err := p.loadMe(r)
	return chart, m, err
}

// formFields copies the named form fields that were sent into body.
func formFields(r *http.Request, body map[string]any, names ...string) {
	for _, f := range names {
		if _, sent := r.PostForm[f]; sent {
			body[f] = strings.TrimSpace(r.PostFormValue(f))
		}
	}
}

// A person's page.

type personPage struct {
	Person     chartPerson
	Reports    []chartPerson
	Leads      []string // teams they lead
	CanArrange bool
	CanRemove  bool
	// For the form: teams they may be moved to, and everyone they could
	// report to.
	Teams    []string
	Managers []string
	// Access: levels that are given (not the ones that come with a place),
	// with whether they have each.
	Access []accessChoice
	// Leading: teams they could be made a lead of, or stop leading.
	LeadTeams []leadChoice
	// For the owner: whether they sign in in two steps, and may it be reset.
	TwoStep, CanResetTwoStep bool
}

type accessChoice struct {
	Name, Describe string
	Has            bool
}

type leadChoice struct {
	Team  string
	Leads bool
}

func (p *Platform) loadPerson(r *http.Request, name string) (personPage, error) {
	chart, m, err := p.orgFor(r)
	if err != nil {
		return personPage{}, err
	}
	i := slices.IndexFunc(chart.People, func(x chartPerson) bool { return x.Kind != "agent" && strings.EqualFold(x.Name, name) })
	if i < 0 {
		return personPage{}, &apiError{Status: http.StatusNotFound, Msg: name + " isn't a person in this org."}
	}
	v := personPage{Person: chart.People[i]}
	name = v.Person.Name
	for _, x := range chart.People {
		if strings.EqualFold(x.ReportsTo, name) {
			v.Reports = append(v.Reports, x)
		}
		if !strings.EqualFold(x.Name, name) {
			v.Managers = append(v.Managers, x.Name)
		}
	}
	for _, t := range chart.Teams {
		if t.LedBy(name) {
			v.Leads = append(v.Leads, t.Name)
		}
	}
	v.CanArrange = m.forPerson("arrange", name)
	v.CanRemove = m.forPerson("invite", name) && !v.Person.Owner
	if m.Owner && !v.Person.Owner && p.opts.TwoSteps != nil {
		t, _ := p.opts.TwoSteps.TwoStepOf(r.Context(), networkOf(r).Name, name)
		v.TwoStep, v.CanResetTwoStep = t.On(), t.On()
	}
	if v.CanArrange {
		v.Teams = m.teams("arrange", chart.Teams)
		if v.Person.Team != "" && !slices.Contains(v.Teams, v.Person.Team) {
			v.Teams = append([]string{v.Person.Team}, v.Teams...)
		}
		var levels accessLevels
		if err := p.runner(r).call(r.Context(), "GET", "/v1/access", nil, &levels); err == nil && v.Person.Kind != "guest" {
			for _, l := range levels.Levels {
				if l.Preset && l.Name != "Admin" {
					continue // Lead, Manager, and Member come with a place
				}
				if l.Name == "Admin" && !m.Owner {
					continue // only the owner makes admins
				}
				v.Access = append(v.Access, accessChoice{Name: l.Name, Describe: l.Describe,
					Has: slices.ContainsFunc(v.Person.Access, func(a string) bool { return strings.EqualFold(a, l.Name) })})
			}
		}
		if v.Person.Kind != "guest" {
			for _, t := range m.teams("arrange", chart.Teams) {
				v.LeadTeams = append(v.LeadTeams, leadChoice{Team: t, Leads: slices.Contains(v.Leads, t)})
			}
		}
	}
	return v, nil
}

func (p *Platform) personPageHandler(w http.ResponseWriter, r *http.Request) {
	p.showPerson(w, r, http.StatusOK, "")
}

func (p *Platform) showPerson(w http.ResponseWriter, r *http.Request, status int, problem string) {
	v, err := p.loadPerson(r, r.PathValue("account"))
	if err != nil {
		p.render(w, r, statusOf(err), "person", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, status, "person", withError{Err: problem, View: v})
}

// afterPerson goes back to someone's page once a change is made, or shows
// what went wrong there.
func (p *Platform) afterPerson(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		p.showPerson(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/people/"+url.PathEscape(r.PathValue("account")), http.StatusSeeOther)
}

func (p *Platform) savePerson(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	formFields(r, body, "name", "role", "team", "reports_to")
	p.afterPerson(w, r, p.runner(r).call(r.Context(), "PATCH", "/v1/members/"+url.PathEscape(r.PathValue("account")), body, nil))
}

func (p *Platform) personAccess(w http.ResponseWriter, r *http.Request) {
	method := "DELETE"
	if r.PostFormValue("give") == "1" {
		method = "POST"
	}
	path := "/v1/people/" + url.PathEscape(r.PathValue("account")) + "/access/" + url.PathEscape(r.PostFormValue("level"))
	p.afterPerson(w, r, p.runner(r).call(r.Context(), method, path, nil, nil))
}

func (p *Platform) personLead(w http.ResponseWriter, r *http.Request) {
	change := "demote"
	if r.PostFormValue("lead") == "1" {
		change = "promote"
	}
	path := "/v1/people/" + url.PathEscape(r.PathValue("account")) + "/" + change
	p.afterPerson(w, r, p.runner(r).call(r.Context(), "POST", path, map[string]any{"team": r.PostFormValue("team")}, nil))
}

func (p *Platform) removePerson(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	if err := p.runner(r).call(r.Context(), "DELETE", "/v1/members/"+url.PathEscape(account), nil, nil); err != nil {
		p.showPerson(w, r, statusOf(err), messageOf(err))
		return
	}
	// Someone given the nick later starts without it.
	n := networkOf(r)
	if p.opts.TwoSteps != nil {
		if err := p.opts.TwoSteps.RemoveTwoStep(r.Context(), n.Name, account); err != nil {
			monitor.Problem(p.opts.Logger, "removing %s's two-step sign-in: %v", account, err)
		}
	}
	// Out of the org is out of the network: their IRC password goes (the
	// console's, and any left in Ergo, which nobody knows the new one of),
	// and so do their sign-ins and open chats.
	if err := p.passwords().RemoveIRCPassword(r.Context(), n.Name, account); err != nil {
		monitor.Problem(p.opts.Logger, "removing %s's password on %s: %v", account, n.Name, err)
	}
	if err := setPassword(r.Context(), n, account, newToken()); err != nil {
		monitor.Problem(p.opts.Logger, "locking %s out of %s's IRC: %v", account, n.Name, err)
	}
	p.signOutEverywhere(r.Context(), n, account)
	http.Redirect(w, r, "/people", http.StatusSeeOther)
}

// Teams.

type teamRow struct {
	team
	Depth int // how many teams it's inside
}

type teamsPage struct {
	Teams []teamRow
	// Making a team: where it may go.
	Parents     []string
	TopLevel    bool
	CanMakeTeam bool
}

// treeOrder lists teams parents first, each followed by the teams inside it.
func treeOrder(all []team) []teamRow {
	byParent := map[string][]team{}
	names := map[string]bool{}
	for _, t := range all {
		names[strings.ToLower(t.Name)] = true
	}
	for _, t := range all {
		parent := strings.ToLower(t.Parent)
		if !names[parent] {
			parent = ""
		}
		byParent[parent] = append(byParent[parent], t)
	}
	var out []teamRow
	seen := map[string]bool{}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, t := range byParent[parent] {
			if seen[strings.ToLower(t.Name)] {
				continue
			}
			seen[strings.ToLower(t.Name)] = true
			out = append(out, teamRow{team: t, Depth: depth})
			walk(strings.ToLower(t.Name), depth+1)
		}
	}
	walk("", 0)
	return out
}

func (p *Platform) loadTeams(r *http.Request) (teamsPage, error) {
	chart, m, err := p.orgFor(r)
	if err != nil {
		return teamsPage{}, err
	}
	v := teamsPage{Teams: treeOrder(chart.Teams), Parents: m.teams("arrange", chart.Teams), TopLevel: m.inTeam("arrange", "")}
	v.CanMakeTeam = v.TopLevel || len(v.Parents) > 0
	return v, nil
}

func (p *Platform) teamsPageHandler(w http.ResponseWriter, r *http.Request) {
	p.showTeams(w, r, http.StatusOK, "")
}

func (p *Platform) showTeams(w http.ResponseWriter, r *http.Request, status int, problem string) {
	v, err := p.loadTeams(r)
	if err != nil {
		p.render(w, r, statusOf(err), "teams", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, status, "teams", withError{Err: problem, View: v})
}

func (p *Platform) makeTeam(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	body := map[string]any{"name": name}
	formFields(r, body, "parent", "focus")
	body["private"] = r.PostFormValue("private") == "on"
	if err := p.runner(r).call(r.Context(), "POST", "/v1/teams", body, nil); err != nil {
		p.showTeams(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/teams/"+url.PathEscape(name), http.StatusSeeOther)
}

// A team's page.

type budgetLine struct {
	Name    string
	Kind    string // human, agent, or team
	Monthly string // "" when it's private
}

type teamPage struct {
	Team       team
	Inside     []team // teams directly inside it
	People     []chartPerson
	Objectives []goal
	Company    []goal // the company's goals, for "serves"
	Lines      []budgetLine
	Committed  string // "" when private
	Left       string
	CanArrange bool
	CanBudget  bool // may set its budget
	CanMove    bool // may move it elsewhere
	Parents    []string
	TopLevel   bool
	Everyone   []string // who could lead it
	Stats      teamStats
	// Repos: the repos GitHub gives the network, and its git repositories on
	// any host (git:<name>), for its leads to choose from.
	Available []string
	// Services: the services connected to the network, likewise.
	Services []connectedService
}

// ServiceTitles are the titles of the connectors the team itself names.
func (v teamPage) ServiceTitles() []string {
	var out []string
	for _, s := range v.Services {
		if v.Team.Uses(s.ID) {
			out = append(out, s.Title)
		}
	}
	return out
}

// teamStats are a team's numbers, counting the teams inside it.
type teamStats struct {
	Humans        int                  `json:"humans"`
	Agents        int                  `json:"agents"`
	Waiting       int                  `json:"waiting"`
	Decided       int                  `json:"decided"`
	MedianMinutes *float64             `json:"median_minutes"`
	LastSaid      map[string]time.Time `json:"last_said"`
	SpendMonth    *float64             `json:"spend_month"`
}

// Typical is how long its approvals typically take to decide, in words.
func (s teamStats) Typical() string {
	if s.MedianMinutes == nil {
		return ""
	}
	m := *s.MedianMinutes
	switch {
	case m < 60:
		return fmt.Sprintf("%.0f min", m)
	case m < 48*60:
		return fmt.Sprintf("%.1f h", m/60)
	}
	return fmt.Sprintf("%.1f days", m/60/24)
}

func (p *Platform) loadTeam(r *http.Request, name string) (teamPage, error) {
	chart, m, err := p.orgFor(r)
	if err != nil {
		return teamPage{}, err
	}
	i := slices.IndexFunc(chart.Teams, func(t team) bool { return strings.EqualFold(t.Name, name) })
	if i < 0 {
		return teamPage{}, &apiError{Status: http.StatusNotFound, Msg: "There's no team " + name + "."}
	}
	v := teamPage{Team: chart.Teams[i]}
	name = v.Team.Name
	for _, t := range chart.Teams {
		if strings.EqualFold(t.Parent, name) {
			v.Inside = append(v.Inside, t)
		}
	}
	for _, x := range chart.People {
		if strings.EqualFold(x.Team, name) {
			v.People = append(v.People, x)
		}
		if x.Kind != "guest" {
			v.Everyone = append(v.Everyone, x.Name)
		}
	}
	p.runner(r).call(r.Context(), "GET", "/v1/teams/"+url.PathEscape(name)+"/stats", nil, &v.Stats)
	var gh struct {
		Repos []string `json:"repos"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/github", nil, &gh); err == nil {
		v.Available = gh.Repos
	}
	for _, g := range p.gitRepoViews(r.Context(), networkOf(r).Name) {
		v.Available = append(v.Available, "git:"+g.Name)
	}
	v.Services, _ = p.services(r)
	var c company
	if err := p.runner(r).call(r.Context(), "GET", "/v1/company", nil, &c); err == nil {
		for _, g := range c.Goals {
			switch {
			case g.Team == "":
				v.Company = append(v.Company, g)
			case strings.EqualFold(g.Team, name):
				v.Objectives = append(v.Objectives, g)
			}
		}
	}
	var b teamBudget
	if err := p.runner(r).call(r.Context(), "GET", "/v1/teams/"+url.PathEscape(name)+"/budget", nil, &b); err == nil {
		for _, c := range b.Commitments {
			line := budgetLine{Name: c.Name, Kind: c.Kind}
			if c.Monthly != nil {
				line.Monthly = usd(*c.Monthly)
			}
			v.Lines = append(v.Lines, line)
		}
		if b.Committed != nil {
			v.Committed = usd(*b.Committed)
		}
		if b.Remaining != nil {
			v.Left = usd(*b.Remaining)
		}
	}
	v.CanArrange = m.inTeam("arrange", name)
	v.CanBudget = (v.Team.Parent == "" && m.Owner) || (v.Team.Parent != "" && m.inTeam("budgets", v.Team.Parent))
	v.CanMove = m.inTeam("arrange", v.Team.Parent)
	for _, t := range m.teams("arrange", chart.Teams) {
		if !insideOf(chart.Teams, t, name) { // not itself, or a team inside it
			v.Parents = append(v.Parents, t)
		}
	}
	v.TopLevel = m.inTeam("arrange", "")
	return v, nil
}

func (p *Platform) teamPageHandler(w http.ResponseWriter, r *http.Request) {
	p.showTeam(w, r, http.StatusOK, "")
}

func (p *Platform) showTeam(w http.ResponseWriter, r *http.Request, status int, problem string) {
	v, err := p.loadTeam(r, r.PathValue("name"))
	if err != nil {
		p.render(w, r, statusOf(err), "team", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, status, "team", withError{Err: problem, View: v})
}

func (p *Platform) saveTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body := map[string]any{}
	formFields(r, body, "focus", "parent", "channel", "chatter")
	if _, sent := r.PostForm["leads"]; sent {
		leads := []string{}
		for _, l := range r.PostForm["leads"] {
			if l = strings.TrimSpace(l); l != "" {
				leads = append(leads, l)
			}
		}
		body["leads"] = leads
	}
	if _, sent := r.PostForm["repos"]; sent {
		repos := []string{}
		for _, x := range r.PostForm["repos"] {
			if x = strings.TrimSpace(x); x != "" {
				repos = append(repos, x)
			}
		}
		body["repos"] = repos
	}
	if _, sent := r.PostForm["connectors"]; sent {
		conns := []string{}
		for _, x := range r.PostForm["connectors"] {
			if x = strings.TrimSpace(x); x != "" {
				conns = append(conns, x)
			}
		}
		body["connectors"] = conns
	}
	if _, sent := r.PostForm["private_sent"]; sent {
		body["private"] = r.PostFormValue("private") == "on"
	}
	if v, sent := r.PostForm["budget"]; sent {
		b, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(v[0]), "$"), 64)
		if err != nil {
			p.showTeam(w, r, http.StatusBadRequest, "The budget is dollars per month, like 2000.")
			return
		}
		body["budget"] = b
	}
	if err := p.runner(r).call(r.Context(), "PATCH", "/v1/teams/"+url.PathEscape(name), body, nil); err != nil {
		p.showTeam(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/teams/"+url.PathEscape(name), http.StatusSeeOther)
}

// The company.

type companyPage struct {
	Company    company
	Goals      []goal // the company's
	Teams      []companyTeam
	CanArrange bool
}

type companyTeam struct {
	teamRow
	Objectives []goal
}

func (p *Platform) loadCompany(r *http.Request) (companyPage, error) {
	chart, m, err := p.orgFor(r)
	if err != nil {
		return companyPage{}, err
	}
	var v companyPage
	if err := p.runner(r).call(r.Context(), "GET", "/v1/company", nil, &v.Company); err != nil {
		return v, err
	}
	for _, g := range v.Company.Goals {
		if g.Team == "" {
			v.Goals = append(v.Goals, g)
		}
	}
	for _, row := range treeOrder(chart.Teams) {
		ct := companyTeam{teamRow: row}
		for _, g := range v.Company.Goals {
			if strings.EqualFold(g.Team, row.Name) {
				ct.Objectives = append(ct.Objectives, g)
			}
		}
		v.Teams = append(v.Teams, ct)
	}
	v.CanArrange = m.inTeam("arrange", "")
	return v, nil
}

func (p *Platform) companyPageHandler(w http.ResponseWriter, r *http.Request) {
	p.showCompany(w, r, http.StatusOK, "")
}

func (p *Platform) showCompany(w http.ResponseWriter, r *http.Request, status int, problem string) {
	v, err := p.loadCompany(r)
	if err != nil {
		p.render(w, r, statusOf(err), "company", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, status, "company", withError{Err: problem, View: v})
}

func (p *Platform) saveCompany(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"description": r.PostFormValue("description")}
	if err := p.runner(r).call(r.Context(), "PATCH", "/v1/company", body, nil); err != nil {
		p.showCompany(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/company", http.StatusSeeOther)
}

// Goals: added and changed from the company's page or a team's.

// goalsBack is where a goal's form goes back to: its team's page, or the
// company's.
func (p *Platform) goalsBack(w http.ResponseWriter, r *http.Request, team string, err error) {
	switch {
	case err != nil && team != "":
		r.SetPathValue("name", team)
		p.showTeam(w, r, statusOf(err), messageOf(err))
	case err != nil:
		p.showCompany(w, r, statusOf(err), messageOf(err))
	case team != "":
		http.Redirect(w, r, "/teams/"+url.PathEscape(team), http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/company", http.StatusSeeOther)
	}
}

func goalBody(r *http.Request) map[string]any {
	body := map[string]any{}
	formFields(r, body, "text", "measure", "target", "current")
	if s := strings.TrimSpace(r.PostFormValue("serves")); s != "" {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			body["serves"] = id
		}
	}
	return body
}

func (p *Platform) addGoal(w http.ResponseWriter, r *http.Request) {
	team := strings.TrimSpace(r.PostFormValue("team"))
	body := goalBody(r)
	if team != "" {
		body["team"] = team
	}
	p.goalsBack(w, r, team, p.runner(r).call(r.Context(), "POST", "/v1/goals", body, nil))
}

func (p *Platform) saveGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		http.NotFound(w, r)
		return
	}
	team := strings.TrimSpace(r.PostFormValue("team"))
	method, body := "PATCH", goalBody(r)
	if r.PostFormValue("drop") == "1" {
		method, body = "DELETE", nil
	}
	p.goalsBack(w, r, team, p.runner(r).call(r.Context(), method, "/v1/goals/"+id, body, nil))
}

// Access levels.

type accessPage struct {
	Levels    []accessLevel
	Abilities []string
	Scopes    []string
	Teams     []string
	Owner     bool
	Mine      []string // the levels the signed-in person holds
}

// Has reports whether level l holds ability a, for the form.
func (l accessLevel) Has(a string) bool { return slices.Contains(l.Abilities, a) }

// InTeam reports whether level l names team t, for the form.
func (l accessLevel) InTeam(t string) bool { return slices.Contains(l.Teams, t) }

func (p *Platform) accessPageHandler(w http.ResponseWriter, r *http.Request) {
	p.showAccess(w, r, http.StatusOK, "")
}

func (p *Platform) showAccess(w http.ResponseWriter, r *http.Request, status int, problem string) {
	chart, m, err := p.orgFor(r)
	var levels accessLevels
	if err == nil {
		err = p.runner(r).call(r.Context(), "GET", "/v1/access", nil, &levels)
	}
	if err != nil {
		p.render(w, r, statusOf(err), "access", withError{Err: messageOf(err)})
		return
	}
	v := accessPage{Levels: levels.Levels, Abilities: levels.Abilities, Scopes: levels.Scopes, Owner: m.Owner, Mine: m.Access}
	for _, t := range chart.Teams {
		v.Teams = append(v.Teams, t.Name)
	}
	p.render(w, r, status, "access", withError{Err: problem, View: v})
}

func (p *Platform) saveAccess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = strings.TrimSpace(r.PostFormValue("name"))
	}
	var err error
	if r.PostFormValue("drop") == "1" {
		err = p.runner(r).call(r.Context(), "DELETE", "/v1/access/"+url.PathEscape(name), nil, nil)
	} else {
		abilities := r.PostForm["abilities"]
		if abilities == nil {
			abilities = []string{}
		}
		body := map[string]any{"abilities": abilities, "scope": r.PostFormValue("scope"), "teams": r.PostForm["teams"]}
		err = p.runner(r).call(r.Context(), "PUT", "/v1/access/"+url.PathEscape(name), body, nil)
	}
	if err != nil {
		p.showAccess(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/access", http.StatusSeeOther)
}

// The org's log.

func (p *Platform) logPage(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Entries []logEntry `json:"entries"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/log?limit=200", nil, &out); err != nil {
		p.render(w, r, statusOf(err), "log", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, http.StatusOK, "log", withError{View: out.Entries})
}

// describeMeasure is how a goal is measured and where it stands: "Weekly
// active customers: 55 of 100".
func describeMeasure(g goal) string {
	var s string
	switch {
	case g.Current != "" && g.Target != "":
		s = g.Current + " of " + g.Target
	case g.Target != "":
		s = "target " + g.Target
	case g.Current != "":
		s = "now " + g.Current
	}
	switch {
	case g.Measure != "" && s != "":
		return g.Measure + ": " + s
	case g.Measure != "":
		return g.Measure
	}
	return s
}

// insideOf reports whether team is outer, or inside it at any depth.
func insideOf(all []team, team, outer string) bool {
	parents := map[string]string{}
	for _, t := range all {
		parents[strings.ToLower(t.Name)] = t.Parent
	}
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[strings.ToLower(cur)]; cur = parents[strings.ToLower(cur)] {
		if strings.EqualFold(cur, outer) {
			return true
		}
		seen[strings.ToLower(cur)] = true
	}
	return false
}
