// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/roles"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
)

// page is what every template gets.
type page struct {
	Network       string
	Account       string
	CSRF          string
	Error         string
	Done          string // "Saved.", once, after a form went through
	Version       string // this deploy's (version.go)
	Nav           string
	HtmxIntegrity string
	Chrome        chrome
	// ChatDock: web chat is docked beside the page (signed in, and web chat
	// is set up here).
	ChatDock bool
	// Adding: the page's form for adding something is open, because it was
	// asked for, or what was sent from the page needs fixing.
	Adding bool
	// Site: guildnet.chat's own header and footer, on its pages.
	Site site
	Data any
}

// chrome is what the console's frame shows around every page: what's waiting
// on you, and whether you may hire.
type chrome struct {
	Waiting  []approval
	CanHire  bool // may hire a new agent somewhere
	CanLog   bool // may see some of the org's log
	CanSpend bool // may see some of what agents spend
	Owner    bool
	// Settings: the owner of a network the host runs (ownedHere).
	Settings bool
	// Server: may see a self-hosted install's Server page.
	Server bool
}

func (p *Platform) loadChrome(r *http.Request) chrome {
	var c chrome
	c.Waiting, _ = p.loadApprovals(r)
	if m, err := p.loadMe(r); err == nil {
		c.Owner = m.Owner
		c.Settings = p.seesSettings(r)
		c.Server, _ = p.serverAccess(r)
		hire, log := m.Abilities["hire"], m.Abilities["log"]
		c.CanHire = m.Owner || hire.Org || len(hire.Teams) > 0
		c.CanLog = m.Owner || log.Org || len(log.Teams) > 0
		c.CanSpend = m.anywhere("spend")
	}
	return c
}

// seesSettings says whether the one signed in owns the request's network, and
// the host runs it: whoever sees its Settings.
func (p *Platform) seesSettings(r *http.Request) bool {
	s, _ := sessionOf(r)
	n := networkOf(r)
	return n.OwnerNick != "" && p.opts.Host != nil && strings.EqualFold(n.OwnerNick, s.Account)
}

func (p *Platform) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	pg := page{Network: networkOf(r).Name, Nav: name, HtmxIntegrity: htmxIntegrity, Version: p.version, Data: data,
		Adding: r.URL.Query().Has("add") || r.Method == http.MethodPost}
	if hostOnly(r.Host) == p.opts.BaseDomain {
		pg.Site = p.siteOf(r)
	}
	if s, ok := sessionOf(r); ok {
		pg.Account, pg.CSRF = s.Account, s.CSRF
		pg.Chrome = p.loadChrome(r)
		// Not beside setting up two-step sign-in (whose recovery codes show
		// once, and mustn't be lost to the console loading afresh as the dock
		// appears), nor for anyone who must set it up first: web chat would
		// refuse them.
		pg.ChatDock = p.opts.ChatDir != "" && name != "two-step"
		if pg.ChatDock {
			first, _ := p.setUpFirst(r)
			pg.ChatDock = !first
		}
	}
	var extra []string
	if pg.ChatDock {
		extra = append(extra, "frame-src 'self'")
	}
	// Forms may go on to the extension's pages (paying, on Stripe's). On
	// every signed-in page: the console's links are boosted, so the page
	// first loaded keeps its policy when Settings is reached from it.
	if pg.Account != "" && len(p.hooks.FormAction) > 0 {
		extra = append(extra, "form-action 'self' "+strings.Join(p.hooks.FormAction, " "))
	}
	if len(extra) > 0 {
		w.Header().Set("Content-Security-Policy", csp(strings.Join(extra, "; ")))
	}
	if e, ok := data.(interface{ ErrorText() string }); ok {
		pg.Error = e.ErrorText()
	}
	if pg.Error == "" && r.Method == http.MethodGet {
		pg.Done = p.doneText(w, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := p.pages[name].Execute(w, pg); err != nil {
		monitor.Problem(p.opts.Logger, "rendering %s: %v", name, err)
	}
}

// withError is page data with a problem to show.
type withError struct {
	Err  string
	View any
}

func (e withError) ErrorText() string { return e.Err }

func (p *Platform) runner(r *http.Request) runner {
	s, _ := sessionOf(r)
	return runner{n: networkOf(r), actor: s.Account}
}

// Signing in.

func (p *Platform) signinPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := sessionOf(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p.render(w, r, http.StatusOK, "signin", withError{})
}

func (p *Platform) signin(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	account := strings.TrimSpace(r.PostFormValue("account"))
	password := r.PostFormValue("password")
	fail := func(status int, msg string) {
		p.render(w, r, status, "signin", withError{Err: msg, View: account})
	}
	if account == "" || password == "" {
		fail(http.StatusBadRequest, "Enter your IRC account name and password.")
		return
	}
	// No IRC account name is longer; anything longer isn't worth counting.
	if len(account) > 64 {
		account = ""
		fail(http.StatusUnauthorized, "That account and password didn't match.")
		return
	}
	// The same check, and the same holding back of guessing, as IRC's.
	res, err := p.checkSignIn(r.Context(), n, account, password, p.visitor(r))
	switch {
	case err != nil:
		monitor.Problem(p.opts.Logger, "checking a password on %s: %v", n.Name, err)
		fail(http.StatusBadGateway, "Couldn't check that just now. Try again in a moment.")
		return
	case res == signInHeld:
		fail(http.StatusTooManyRequests, "Too many wrong passwords. Wait 15 minutes, then try again.")
		return
	case res == signInNotAPerson:
		// An IRC account that isn't a person's in the org: with its right
		// password, they're told why they can't come in.
		if right, err := checkPassword(r.Context(), n, account, password); err == nil && right {
			fail(http.StatusForbidden, fmt.Sprintf("%s isn't in %s's org. Ask the owner to add you.", account, n.Name))
			return
		}
		p.passwords().SignInFailed(r.Context(), n.Name, strings.ToLower(account), signInSource(p.visitor(r)), p.opts.Now())
		fail(http.StatusUnauthorized, "That account and password didn't match.")
		return
	case res != signInRight:
		fail(http.StatusUnauthorized, "That account and password didn't match.")
		return
	}
	// A password isn't enough: they must be a person in this org.
	if err := (runner{n: n, actor: account}).call(r.Context(), "GET", "/v1/org", nil, nil); err != nil {
		if statusOf(err) == http.StatusForbidden {
			fail(http.StatusForbidden, fmt.Sprintf("%s isn't in %s's org. Ask the owner to add you.", account, n.Name))
			return
		}
		fail(http.StatusBadGateway, messageOf(err))
		return
	}
	if err := p.signInAs(w, r, n, account); err != nil {
		monitor.Problem(p.opts.Logger, "signing %s in: %v", account, err)
		fail(http.StatusInternalServerError, "Couldn't sign you in. Try again in a moment.")
	}
}

// notFound is any address the console has no page for.
func (p *Platform) notFound(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusNotFound, "not-found", nil)
}

func (p *Platform) signout(w http.ResponseWriter, r *http.Request) {
	p.endSession(w, r)
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

// The org chart, and hiring.

type personView struct {
	Name, Role string
	Agent      bool
	Owner      bool
	Lead       bool
}

type teamView struct {
	Name, Channel string
	Private       bool
	People        []personView
}

type orgView struct {
	// ByTeam: shown as team cards; Picture: drawn as a chart of boxes and
	// lines; otherwise, the reporting tree as a list.
	ByTeam  bool
	Picture bool
	// Hiring: the role catalogue by department, and the role to start from.
	Roles     []roleGroup
	Preset    string
	Desks     deskKinds // the desks (workspaces with tools) agents here can have
	Legend    []teamColor
	Tree      []*chartNode
	Teams     []teamView
	HireTeams []string // teams the viewer may hire into
	CanHire   bool
	// Models to hire on, by tier, and the one a new hire gets without a role.
	Models       []modelGroup
	DefaultModel string
	Checklist    *checklistView // the owner's getting started, until it's done
	Notice       template.HTML  // the extension's, for the owner (Hooks.Notice)
	// FirstHire: no agents yet, so the hiring form starts open.
	FirstHire bool
}

func (p *Platform) loadOrg(r *http.Request) (orgView, error) {
	var chart orgChart
	if err := p.runner(r).call(r.Context(), "GET", "/v1/org", nil, &chart); err != nil {
		return orgView{}, err
	}
	s, _ := sessionOf(r)
	owner := strings.EqualFold(chart.Owner, s.Account)
	byTeam := map[string]*teamView{}
	var v orgView
	for _, l := range chart.Teams {
		v.Teams = append(v.Teams, teamView{Name: l.Name, Channel: l.Channel, Private: l.Private})
		if owner || l.LedBy(s.Account) {
			v.HireTeams = append(v.HireTeams, l.Name)
		}
	}
	v.Teams = append(v.Teams, teamView{Name: "No team"})
	for i := range v.Teams {
		byTeam[strings.ToLower(v.Teams[i].Name)] = &v.Teams[i]
	}
	teams := map[string]team{}
	for _, l := range chart.Teams {
		teams[strings.ToLower(l.Name)] = l
	}
	for _, person := range chart.People {
		lv := byTeam[strings.ToLower(person.Team)]
		if lv == nil {
			lv = byTeam["no team"]
		}
		lv.People = append(lv.People, personView{Name: person.Name, Role: person.Role, Agent: person.Kind == "agent",
			Owner: person.Owner, Lead: teams[strings.ToLower(person.Team)].LedBy(person.Name)})
	}
	for i := range v.Teams {
		sort.SliceStable(v.Teams[i].People, func(a, b int) bool {
			pa, pb := v.Teams[i].People[a], v.Teams[i].People[b]
			if pa.Lead != pb.Lead {
				return pa.Lead
			}
			return pa.Agent == pb.Agent && pa.Name < pb.Name || !pa.Agent && pb.Agent
		})
	}
	if last := v.Teams[len(v.Teams)-1]; len(last.People) == 0 {
		v.Teams = v.Teams[:len(v.Teams)-1]
	}
	v.CanHire = len(v.HireTeams) > 0
	v.FirstHire = !slices.ContainsFunc(chart.People, func(x chartPerson) bool { return x.Kind == "agent" })
	v.ByTeam = r.URL.Query().Get("view") == "teams"
	v.Picture = r.URL.Query().Get("view") == "picture"
	v.Roles, v.Preset = roleGroups(), r.URL.Query().Get("preset")
	if v.CanHire {
		v.Models, v.DefaultModel = p.loadModels(r, "")
	}
	v.Desks = p.desksHere(r)
	for i, t := range chart.Teams {
		v.Legend = append(v.Legend, teamColor{Name: t.Name, Color: i % teamColors})
	}
	v.Tree = p.reportingTree(r, chart, s.Account)
	if owner {
		v.Checklist = p.checklist(r, chart)
	}
	if p.seesSettings(r) {
		v.Notice = p.notice(r, networkOf(r))
	}
	return v, nil
}

func (p *Platform) orgPage(w http.ResponseWriter, r *http.Request) {
	v, err := p.loadOrg(r)
	if err != nil {
		p.render(w, r, statusOf(err), "org", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, http.StatusOK, "org", withError{View: v})
}

func (p *Platform) hire(w http.ResponseWriter, r *http.Request) {
	nick := strings.TrimSpace(r.PostFormValue("nick"))
	body := map[string]any{"nick": nick}
	for _, f := range []string{"preset", "role", "team", "model", "platform"} {
		if v := strings.TrimSpace(r.PostFormValue(f)); v != "" {
			body[f] = v
		}
	}
	if v := strings.TrimSpace(r.PostFormValue("budget")); v != "" {
		b, err := strconv.ParseFloat(v, 64)
		if err != nil {
			p.showOrgError(w, r, http.StatusBadRequest, "Budget is dollars per day, like 1.50.")
			return
		}
		body["budget"] = b
	}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/staff", body, nil); err != nil {
		p.showOrgError(w, r, statusOf(err), messageOf(err))
		return
	}
	if body["platform"] == "outside" {
		// Its program needs a token to reach the network: shown now, once.
		p.showAgentToken(w, r, nick)
		return
	}
	http.Redirect(w, r, "/staff/"+url.PathEscape(nick), http.StatusSeeOther)
}

// tokenView is an outside agent's new token, shown once, with how its
// program uses it.
type tokenView struct {
	// Docs is where the docs for outside agents are, if anywhere.
	Nick, Token, API, Models, Docs string
}

// showAgentToken makes an outside agent a new token and shows it, once.
func (p *Platform) showAgentToken(w http.ResponseWriter, r *http.Request, nick string) {
	var out struct {
		Token string `json:"token"`
	}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/staff/"+url.PathEscape(nick)+"/token", nil, &out); err != nil {
		p.showAgentError(w, r, statusOf(err), messageOf(err))
		return
	}
	scheme := "http"
	if p.opts.SecureCookies {
		scheme = "https"
	}
	p.render(w, r, http.StatusOK, "agent-token", withError{View: tokenView{Nick: nick, Token: out.Token, API: scheme + "://" + r.Host + "/agents/v1", Models: p.modelsAddress(r), Docs: p.agentDocs()}})
}

// hookView is an outside agent's new webhook, with its secret shown once.
type hookView struct {
	Nick, URL, Secret string
}

// setAgentWebhook sends an outside agent's work to an address of its own,
// or back to its inbox; a new address's signing secret is shown once.
func (p *Platform) setAgentWebhook(w http.ResponseWriter, r *http.Request) {
	nick, addr := r.PathValue("nick"), strings.TrimSpace(r.PostFormValue("webhook_url"))
	var out struct {
		Secret string `json:"secret"`
	}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/staff/"+url.PathEscape(nick)+"/webhook", map[string]string{"url": addr}, &out); err != nil {
		p.showAgentError(w, r, statusOf(err), messageOf(err))
		return
	}
	if out.Secret == "" {
		http.Redirect(w, r, "/staff/"+url.PathEscape(nick), http.StatusSeeOther)
		return
	}
	p.render(w, r, http.StatusOK, "agent-webhook", withError{View: hookView{Nick: nick, URL: addr, Secret: out.Secret}})
}

// newAgentToken replaces an outside agent's token, from its page.
func (p *Platform) newAgentToken(w http.ResponseWriter, r *http.Request) {
	p.showAgentToken(w, r, r.PathValue("nick"))
}

func (p *Platform) showOrgError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	v, _ := p.loadOrg(r)
	p.render(w, r, status, "org", withError{Err: msg, View: v})
}

// deskKinds are the desks agents on a network can have: GuildNet's own (in
// its sandbox, any model), and Managed Agents' (Claude only).
type deskKinds struct {
	Own, Managed bool
}

// desksHere asks the network's runner which desks its agents can have.
func (p *Platform) desksHere(r *http.Request) deskKinds {
	var gh struct {
		Managed bool `json:"desks"`
		Own     bool `json:"own_desks"`
	}
	if p.runner(r).call(r.Context(), "GET", "/v1/github", nil, &gh) != nil {
		return deskKinds{}
	}
	return deskKinds{Own: gh.Own, Managed: gh.Managed}
}

// An agent's page, and changing or firing it.

type staffView struct {
	Agent   agentProfile
	CanEdit bool
	Prompt  string
	// Desks are the desks it can be moved to.
	Desks deskKinds
	// For the form: the teams it may be moved to (its own, and wherever the
	// person looking may hire), and everyone it could report to.
	Teams    []string
	Managers []string
	// Models it can be moved to, by tier; Kept is its own when that's no
	// longer offered (an older one), so saving the form keeps it.
	Models []modelGroup
	Kept   bool
	// Where an outside agent's program calls models, and whether it can.
	Gateway   string
	GatewayOn bool
	// Work is its recent wake-ups, when the person looking may see them.
	Work []workDone
}

// workDone is one of an agent's wake-ups, as its page shows it.
type workDone struct {
	Where    string     `json:"where"`
	Asker    string     `json:"asker"`
	Asked    string     `json:"asked"`
	Started  time.Time  `json:"started"`
	Finished time.Time  `json:"finished"`
	Outcome  string     `json:"outcome"`
	Cost     *float64   `json:"cost"`
	Steps    []workStep `json:"steps"`
}

type workStep struct {
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Result  bool      `json:"result"`
}

// Took is how long it took: 38s, 4m 12s.
func (w workDone) Took() string {
	d := w.Finished.Sub(w.Started).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Since is how far into the work a step came: +42s, +3m 10s.
func (w workDone) Since(s workStep) string {
	d := s.At.Sub(w.Started).Round(time.Second)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("+%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("+%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Spent is what it cost, if the person looking may see that: under a cent
// says so rather than $0.00.
func (w workDone) Spent() string {
	switch {
	case w.Cost == nil:
		return ""
	case *w.Cost > 0 && *w.Cost < 0.01:
		return "under $0.01"
	}
	return usd(*w.Cost)
}

// Results are what its steps achieved.
func (w workDone) Results() []string {
	var out []string
	for _, s := range w.Steps {
		if s.Result {
			out = append(out, s.Summary)
		}
	}
	return out
}

// modelGroup is a tier's models to pick from.
type modelGroup struct {
	Tier   string
	Models []modelChoice
}

type modelChoice struct {
	ID, Label string
	Selected  bool
}

// loadModels is the models agents here can be on, by tier, with current
// selected, and the title of the one a new hire gets.
func (p *Platform) loadModels(r *http.Request, current string) (groups []modelGroup, defaultTitle string) {
	var out struct {
		Models []struct {
			ID      string      `json:"id"`
			Title   string      `json:"title"`
			Tier    models.Tier `json:"tier"`
			Pricing string      `json:"pricing"`
		} `json:"models"`
		Default string `json:"default"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/models", nil, &out); err != nil {
		return nil, ""
	}
	for _, m := range out.Models {
		if m.ID == out.Default {
			defaultTitle = m.Title
		}
		if len(groups) == 0 || groups[len(groups)-1].Tier != m.Tier.Title() {
			groups = append(groups, modelGroup{Tier: m.Tier.Title()})
		}
		g := &groups[len(groups)-1]
		g.Models = append(g.Models, modelChoice{ID: m.ID, Label: modelLabel(m.Title, m.Pricing), Selected: m.ID == current})
	}
	return groups, defaultTitle
}

func (p *Platform) loadAgent(r *http.Request, nick string) (staffView, error) {
	var a agentProfile
	if err := p.runner(r).call(r.Context(), "GET", "/v1/staff/"+url.PathEscape(nick), nil, &a); err != nil {
		return staffView{}, err
	}
	v := staffView{Agent: a, CanEdit: a.Prompt != nil}
	if a.Platform == "outside" {
		if m := p.hooks.Models; m != nil {
			v.Gateway, v.GatewayOn = p.modelsAddress(r), m.On(r, networkOf(r))
		}
	}
	if a.Prompt != nil {
		v.Prompt = *a.Prompt
	}
	var work struct {
		Work []workDone `json:"work"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/staff/"+url.PathEscape(nick)+"/work", nil, &work); err == nil {
		v.Work = work.Work
	}
	if !v.CanEdit {
		return v, nil
	}
	chart, m, err := p.orgFor(r)
	if err != nil {
		return v, nil
	}
	v.Desks = p.desksHere(r)
	v.Models, _ = p.loadModels(r, a.Model)
	v.Kept = len(v.Models) > 0 && !slices.ContainsFunc(v.Models, func(g modelGroup) bool {
		return slices.ContainsFunc(g.Models, func(c modelChoice) bool { return c.Selected })
	})
	v.Teams = m.teams("hire", chart.Teams)
	if a.Team != "" && !slices.Contains(v.Teams, a.Team) {
		v.Teams = append([]string{a.Team}, v.Teams...)
	}
	for _, x := range chart.People {
		if !strings.EqualFold(x.Name, a.Nick) && x.Kind != "guest" {
			v.Managers = append(v.Managers, x.Name)
		}
	}
	return v, nil
}

func (p *Platform) staffPage(w http.ResponseWriter, r *http.Request) {
	v, err := p.loadAgent(r, r.PathValue("nick"))
	if err != nil {
		p.render(w, r, statusOf(err), "staff", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, http.StatusOK, "staff", withError{View: v})
}

func (p *Platform) showAgentError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	v, _ := p.loadAgent(r, r.PathValue("nick"))
	p.render(w, r, status, "staff", withError{Err: msg, View: v})
}

func (p *Platform) saveAgent(w http.ResponseWriter, r *http.Request) {
	nick := r.PathValue("nick")
	body := map[string]any{}
	for _, f := range []string{"name", "role", "model", "effort", "prompt", "platform", "team", "reports_to"} {
		if _, sent := r.PostForm[f]; sent {
			body[f] = strings.TrimSpace(r.PostFormValue(f))
		}
	}
	if _, sent := r.PostForm["budget"]; sent {
		b, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("budget")), 64)
		if err != nil {
			p.showAgentError(w, r, http.StatusBadRequest, "Budget is dollars per day, like 1.50.")
			return
		}
		body["budget"] = b
	}
	if _, sent := r.PostForm["daily_wakes"]; sent {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("daily_wakes")))
		if err != nil {
			p.showAgentError(w, r, http.StatusBadRequest, "Wake-ups a day is a whole number, like 100.")
			return
		}
		body["daily_wakes"] = n
	}
	if _, sent := r.PostForm["channels"]; sent {
		var chans []string
		for _, c := range strings.Split(r.PostFormValue("channels"), ",") {
			if c = strings.TrimSpace(c); c != "" {
				chans = append(chans, c)
			}
		}
		body["channels"] = chans
	}
	if err := p.runner(r).call(r.Context(), "PATCH", "/v1/staff/"+url.PathEscape(nick), body, nil); err != nil {
		p.showAgentError(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/staff/"+url.PathEscape(nick), http.StatusSeeOther)
}

func (p *Platform) fire(w http.ResponseWriter, r *http.Request) {
	nick := r.PathValue("nick")
	body := map[string]any{"reason": strings.TrimSpace(r.PostFormValue("reason"))}
	if err := p.runner(r).call(r.Context(), "DELETE", "/v1/staff/"+url.PathEscape(nick), body, nil); err != nil {
		p.showAgentError(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Approvals.

func (p *Platform) loadApprovals(r *http.Request) ([]approval, error) {
	var out struct {
		Approvals []approval `json:"approvals"`
	}
	err := p.runner(r).call(r.Context(), "GET", "/v1/approvals", nil, &out)
	return out.Approvals, err
}

type approvalsView struct {
	Approvals []approval
	MoveAfter string // how long one waits on each person: "4h", or "off"
	Owner     bool
}

func (p *Platform) loadApprovalsPage(r *http.Request) (approvalsView, error) {
	var out struct {
		Approvals []approval `json:"approvals"`
		MoveAfter string     `json:"move_after"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/approvals", nil, &out); err != nil {
		return approvalsView{}, err
	}
	m, _ := p.loadMe(r)
	return approvalsView{Approvals: out.Approvals, MoveAfter: out.MoveAfter, Owner: m.Owner}, nil
}

func (p *Platform) approvalsPage(w http.ResponseWriter, r *http.Request) {
	p.showApprovals(w, r, http.StatusOK, "")
}

func (p *Platform) showApprovals(w http.ResponseWriter, r *http.Request, status int, problem string) {
	v, err := p.loadApprovalsPage(r)
	if err != nil {
		p.render(w, r, statusOf(err), "approvals", withError{Err: messageOf(err)})
		return
	}
	p.render(w, r, status, "approvals", withError{Err: problem, View: v})
}

// setMoveAfter is the owner setting how long an approval waits on one person.
func (p *Platform) setMoveAfter(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"after": strings.TrimSpace(r.PostFormValue("after"))}
	if err := p.runner(r).call(r.Context(), "PUT", "/v1/approvals/move-after", body, nil); err != nil {
		p.showApprovals(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/approvals", http.StatusSeeOther)
}
func (p *Platform) decide(w http.ResponseWriter, r *http.Request) {
	id, decision := r.PathValue("id"), r.PathValue("decision")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil || (decision != "approve" && decision != "deny") {
		http.NotFound(w, r)
		return
	}
	body := map[string]any{"reason": strings.TrimSpace(r.PostFormValue("reason"))}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/approvals/"+id+"/"+decision, body, nil); err != nil {
		p.showApprovals(w, r, statusOf(err), messageOf(err))
		return
	}
	// Back to where they decided: the org chart or the approvals page, and
	// nowhere else.
	back := "/approvals"
	if r.PostFormValue("back") == "/" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// usd is dollars as people write them: $1.50, $1,250.75, and round sums of
// a hundred or more without the cents, $20,000.
func usd(v float64) string {
	s := fmt.Sprintf("%.2f", math.Abs(v))
	whole, cents, _ := strings.Cut(s, ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	sign := ""
	if v < 0 && s != "0.00" {
		sign = "-"
	}
	if cents == "00" && math.Abs(v) >= 100 {
		return sign + "$" + whole
	}
	return sign + "$" + whole + "." + cents
}

// initial is the letter on someone's avatar.
func initial(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// The reporting tree.

// chartNode is someone on the org chart, with who reports to them.
type chartNode struct {
	chartPerson
	Lead       bool // leads a team
	Me         bool // the person looking
	Reports    []*chartNode
	SpendToday string // "$0.25 of $1.00 today", for agents whose spend they may see
	Color      int    // their team's colour in the picture, or -1 for none
}

// teamColors is how many colours teams take turns with in the picture.
const teamColors = 8

type teamColor struct {
	Name  string
	Color int
}

// Agent reports whether they're an AI agent (for the avatar).
func (n *chartNode) Agent() bool { return n.Kind == "agent" }

// reportingTree arranges the org by who reports to whom: the owner first,
// then anyone whose manager isn't in the org; each person's reports under
// them, people before agents, then by name.
func (p *Platform) reportingTree(r *http.Request, chart orgChart, account string) []*chartNode {
	spend := map[string]string{}
	var out struct {
		Agents []struct {
			Nick        string  `json:"nick"`
			DailyBudget float64 `json:"daily_budget"`
			Today       float64 `json:"today"`
		} `json:"agents"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/spend", nil, &out); err == nil {
		for _, a := range out.Agents {
			spend[strings.ToLower(a.Nick)] = usd(a.Today) + " of " + usd(a.DailyBudget) + " today"
		}
	}
	nodes := map[string]*chartNode{}
	for _, x := range chart.People {
		n := &chartNode{chartPerson: x, Me: strings.EqualFold(x.Name, account), SpendToday: spend[strings.ToLower(x.Name)], Color: -1}
		for i, t := range chart.Teams {
			n.Lead = n.Lead || t.LedBy(x.Name)
			if strings.EqualFold(t.Name, x.Team) {
				n.Color = i % teamColors
			}
		}
		nodes[strings.ToLower(x.Name)] = n
	}
	var roots []*chartNode
	for _, x := range chart.People {
		n := nodes[strings.ToLower(x.Name)]
		boss := nodes[strings.ToLower(x.ReportsTo)]
		if x.Owner || boss == nil || boss == n || reportsUpTo(nodes, x.ReportsTo, x.Name) {
			roots = append(roots, n)
			continue
		}
		boss.Reports = append(boss.Reports, n)
	}
	var order func([]*chartNode)
	order = func(ns []*chartNode) {
		sort.SliceStable(ns, func(i, j int) bool {
			a, b := ns[i], ns[j]
			if a.Owner != b.Owner {
				return a.Owner
			}
			if (a.Kind == "agent") != (b.Kind == "agent") {
				return b.Kind == "agent"
			}
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		})
		for _, n := range ns {
			order(n.Reports)
		}
	}
	order(roots)
	return roots
}

// reportsUpTo reports whether manager's line leads up to name: a loop, which
// the runner refuses, drawn from the top rather than lost.
func reportsUpTo(nodes map[string]*chartNode, manager, name string) bool {
	seen := map[string]bool{}
	for cur := strings.ToLower(manager); cur != "" && !seen[cur]; {
		if cur == strings.ToLower(name) {
			return true
		}
		seen[cur] = true
		n := nodes[cur]
		if n == nil {
			return false
		}
		cur = strings.ToLower(n.ReportsTo)
	}
	return false
}

// The role catalogue.

type roleGroup struct {
	Department string
	Roles      []roles.Role
}

func roleGroups() []roleGroup {
	var out []roleGroup
	for _, d := range roles.Departments() {
		out = append(out, roleGroup{Department: d, Roles: roles.In(d)})
	}
	return out
}

func (p *Platform) rolesPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, "roles", withError{View: roleGroups()})
}
