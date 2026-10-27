// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/internal/roles"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
)

// The runner's internal API, for the platform's console. It's for the
// platform only, never the public: every call carries this network's secret
// (so another network's secret gets nothing) and names the person acting,
// who must be a person in this org. The same rules as GuildServ then apply
// to that person, so the console can never do more than they could in IRC.

// actorHeader names the person a call acts for. Only the platform, holding
// the network's secret, can say who that is.
const actorHeader = "GuildNet-Actor"

type actorKey struct{}

// checkAPIConfig refuses to serve the API without a secret.
func (r *Runner) checkAPIConfig() error {
	if r.cfg.API.Listen != "" && r.cfg.API.Token == "" {
		return errors.New("the runner's API needs this network's secret (GUILDNET_API_TOKEN) before it will listen")
	}
	return nil
}

// startAPI serves the API, if it's configured.
func (r *Runner) startAPI() error {
	if r.cfg.API.Listen == "" {
		return nil
	}
	ln, err := net.Listen("tcp", r.cfg.API.Listen)
	if err != nil {
		return fmt.Errorf("API: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/org", r.apiOrg)
	mux.HandleFunc("GET /v1/github", func(w http.ResponseWriter, _ *http.Request) {
		account, repos := r.githubInstallation()
		if repos == nil {
			repos = []string{}
		}
		hp, ok := r.brain.(interface{ HasPlatform(string) bool })
		apiJSON(w, http.StatusOK, map[string]any{"connected": account != "" && len(repos) > 0, "account": account, "repos": repos,
			"desks": ok && hp.HasPlatform(PlatformManaged), "own_desks": ok && hp.HasPlatform(PlatformDesk)})
	})
	// After the network's model keys change, the platform has the runner
	// fetch them again (it also does every few minutes).
	mux.HandleFunc("POST /v1/model-keys/refresh", func(w http.ResponseWriter, _ *http.Request) {
		if r.cfg.RefreshModelKeys != nil {
			r.cfg.RefreshModelKeys()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/roles", func(w http.ResponseWriter, _ *http.Request) {
		// Each with the model its tier gives here.
		type roleHere struct {
			roles.Role
			Model string `json:"model"`
		}
		here := make([]roleHere, 0, len(roles.All))
		for _, role := range roles.All {
			m, _ := models.ForTier(role.Tier, r.cfg.ModelProviders())
			here = append(here, roleHere{role, m.ID})
		}
		apiJSON(w, http.StatusOK, map[string]any{"roles": here, "departments": roles.Departments()})
	})
	// The models agents here can be hired on or moved to, cheapest tier
	// first, and the one a new hire gets.
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		type modelHere struct {
			ID      string      `json:"id"`
			Title   string      `json:"title"`
			Tier    models.Tier `json:"tier"`
			Pricing string      `json:"pricing"`
		}
		offered := models.Offered(r.cfg.ModelProviders())
		out := make([]modelHere, 0, len(offered))
		for _, m := range offered {
			out = append(out, modelHere{m.ID, m.Title, m.Tier, m.Pricing(r.now())})
		}
		apiJSON(w, http.StatusOK, map[string]any{"models": out, "default": r.defaultModel()})
	})
	mux.HandleFunc("GET /v1/me/channels", func(w http.ResponseWriter, req *http.Request) {
		channels := r.personsChannels(actorOf(req))
		apiJSON(w, http.StatusOK, map[string]any{"channels": channels, "repos": r.channelsRepos(channels), "agents": r.agentsIn(channels)})
	})
	mux.HandleFunc("GET /v1/staff/{nick}", r.apiAgent)
	mux.HandleFunc("POST /v1/staff", r.apiHire)
	mux.HandleFunc("PATCH /v1/staff/{nick}", r.apiSet)
	mux.HandleFunc("DELETE /v1/staff/{nick}", r.apiFire)
	mux.HandleFunc("POST /v1/teams", r.apiCreateTeam)
	mux.HandleFunc("PATCH /v1/teams/{name}", r.apiSetTeam)
	mux.HandleFunc("GET /v1/teams/{name}/budget", r.apiBudget)
	mux.HandleFunc("GET /v1/teams/{name}/stats", r.apiTeamStats)
	mux.HandleFunc("GET /v1/members/{account}", r.apiMember)
	mux.HandleFunc("POST /v1/members", r.apiAddMember)
	mux.HandleFunc("PATCH /v1/members/{account}", r.apiSetMember)
	mux.HandleFunc("DELETE /v1/members/{account}", r.apiRemoveMember)
	mux.HandleFunc("POST /v1/people/{name}/{change}", r.apiPromote)
	mux.HandleFunc("POST /v1/people/{name}/access/{level}", r.apiGiveAccess)
	mux.HandleFunc("DELETE /v1/people/{name}/access/{level}", r.apiGiveAccess)
	mux.HandleFunc("GET /v1/me", r.apiMe)
	mux.HandleFunc("GET /v1/access", r.apiAccessLevels)
	mux.HandleFunc("PUT /v1/access/{name}", r.apiSetAccessLevel)
	mux.HandleFunc("DELETE /v1/access/{name}", r.apiDropAccessLevel)
	mux.HandleFunc("GET /v1/log", r.apiLog)
	mux.HandleFunc("GET /v1/company", r.apiCompany)
	mux.HandleFunc("PATCH /v1/company", r.apiSetCompany)
	mux.HandleFunc("POST /v1/goals", r.apiAddGoal)
	mux.HandleFunc("PATCH /v1/goals/{id}", r.apiSetGoal)
	mux.HandleFunc("DELETE /v1/goals/{id}", r.apiDropGoal)
	mux.HandleFunc("GET /v1/approvals", r.apiApprovals)
	mux.HandleFunc("POST /v1/approvals/{id}/{decision}", r.apiDecide)
	mux.HandleFunc("PUT /v1/approvals/move-after", r.apiSetMoveAfter)
	mux.HandleFunc("POST /v1/guildserv", r.apiGuildServ)
	mux.HandleFunc("GET /v1/stats", r.apiStats)
	mux.HandleFunc("GET /v1/feedback", r.apiFeedback)
	mux.HandleFunc("GET /v1/spend", r.apiSpend)
	mux.HandleFunc("POST /v1/staff/{nick}/token", r.apiAgentToken)
	mux.HandleFunc("POST /v1/staff/{nick}/webhook", r.apiAgentWebhook)
	mux.HandleFunc("GET /v1/staff/{nick}/work", r.apiWork)
	mux.HandleFunc("GET /v1/connectors", r.apiConnectors)
	mux.HandleFunc("POST /v1/connectors/{connector}/tools/{tool}", r.apiSetConnectorTool)
	mux.HandleFunc("GET /v1/desk-hosts", r.apiDeskHosts)
	mux.HandleFunc("POST /v1/desk-hosts", r.apiAddDeskHost)
	mux.HandleFunc("POST /v1/desk-hosts/ask", r.apiAskForDeskHost)
	mux.HandleFunc("DELETE /v1/desk-hosts/{host}", r.apiRemoveDeskHost)
	// Outside agents' programs, through the platform: their own token says
	// who's calling, instead of a person.
	agents := http.NewServeMux()
	r.agentRoutes(agents)
	r.agentToolRoutes(agents)
	people, outsiders := r.apiAuth(mux), r.agentAuth(agents)
	r.api = &http.Server{Handler: monitor.Handler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if apiStarting(r, req) {
			apiError(w, http.StatusServiceUnavailable, "the network is starting: try again in a moment")
			return
		}
		if strings.HasPrefix(req.URL.Path, "/agents/") {
			outsiders.ServeHTTP(w, req)
			return
		}
		// And their model calls, if there's a model gateway (modelcalls.go).
		if strings.HasPrefix(req.URL.Path, "/models/") {
			if calls := r.modelCalls(); calls != nil {
				calls.ServeHTTP(w, req)
			} else {
				http.NotFound(w, req)
			}
			return
		}
		people.ServeHTTP(w, req)
	})), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	r.apiAddr = ln.Addr().String()
	go r.api.Serve(ln)
	r.logger.Printf("API listening on %s", r.apiAddr)
	return nil
}

// apiStarting says req must wait for Start to finish: until everyone's
// connected, only who someone is (GET /v1/me) is answered, which needs only
// the org chart, and which the console asks while Ergo checks agents'
// logins.
func apiStarting(r *Runner, req *http.Request) bool {
	return !r.started.Load() && !(req.Method == "GET" && req.URL.Path == "/v1/me")
}

// APIAddr is where the API listens, once started ("" if it's off).
func (r *Runner) APIAddr() string { return r.apiAddr }

func (r *Runner) stopAPI() {
	if r.api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.api.Shutdown(ctx)
	}
}

// apiAuth lets a call through only with this network's secret and a person
// in the org acting.
func (r *Runner) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(r.cfg.API.Token)) != 1 {
			apiError(w, http.StatusUnauthorized, "this network's secret is required")
			return
		}
		// For GuildNet's admins, who aren't in the org, the secret is enough:
		// stats (numbers only) and #feedback (said there to reach them).
		if req.Method == "GET" && (req.URL.Path == "/v1/stats" || req.URL.Path == "/v1/feedback") {
			next.ServeHTTP(w, req)
			return
		}
		actor := strings.TrimSpace(req.Header.Get(actorHeader))
		if actor == "" {
			apiError(w, http.StatusUnauthorized, "say who is acting, in the "+actorHeader+" header")
			return
		}
		if !r.inOrgAsPerson(actor) {
			apiError(w, http.StatusForbidden, actor+" isn't a person in this org")
			return
		}
		if r.isGuest(actor) && !guestMay(req, actor) {
			apiError(w, http.StatusForbidden, "guests see only their own channels and the agents in them")
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), actorKey{}, actor)))
	})
}

// guestMay reports whether a guest may make the call: nothing of the
// company's directory (its staff, teams, budgets, goals), only who they are,
// their channels and the agents in them, GuildServ (which gives guests only
// HELP and CATCHUP), and disconnecting their own IRC apps.
func guestMay(req *http.Request, actor string) bool {
	switch req.Method + " " + req.URL.Path {
	case "GET /v1/me", "GET /v1/me/channels", "POST /v1/guildserv":
		return true
	}
	return req.Method == "POST" && strings.EqualFold(req.URL.Path, "/v1/people/"+actor+"/disconnect")
}

func actorOf(req *http.Request) string {
	a, _ := req.Context().Value(actorKey{}).(string)
	return a
}

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	apiJSON(w, status, map[string]string{"error": msg})
}

// apiRefusal answers with a refusal's message and the matching status.
func apiRefusal(w http.ResponseWriter, err error) {
	status := map[refusalKind]int{
		invalid:   http.StatusBadRequest,
		forbidden: http.StatusForbidden,
		notFound:  http.StatusNotFound,
		conflict:  http.StatusConflict,
		failed:    http.StatusInternalServerError,
		tooMany:   http.StatusTooManyRequests,
	}[kindOf(err)]
	apiError(w, status, err.Error())
}

// decode reads a JSON body into v; an empty body is fine.
func decode(w http.ResponseWriter, req *http.Request, v any) bool {
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		apiError(w, http.StatusBadRequest, "couldn't read the request: "+err.Error())
		return false
	}
	return true
}

// agentJSON is an agent as the API shows it. The prompt is only shown to
// people who may change the agent.
type agentJSON struct {
	Nick      string `json:"nick"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Team      string `json:"team"`
	ReportsTo string `json:"reports_to"`
	Model     string `json:"model"`
	// ModelTitle and ModelPricing are its model for people: "Claude Sonnet
	// 5.5", "$2 in, $10 out per million tokens".
	ModelTitle   string   `json:"model_title"`
	ModelPricing string   `json:"model_pricing,omitempty"`
	Effort       string   `json:"effort"`
	Budget       float64  `json:"budget"`
	Channels     []string `json:"channels"`
	Platform     string   `json:"platform"`
	Prompt       *string  `json:"prompt,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	// For an agent outside GuildNet: whether its program is checking in, and
	// when it last did.
	Connected  *bool      `json:"connected,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	DailyWakes int        `json:"daily_wakes,omitempty"`
	WakesToday *int       `json:"wakes_today,omitempty"`
	// How it gets its work: "inbox" (its program checks in) or "webhook"
	// (sent to WebhookURL; LastDelivery says how the last call went).
	GetsWork     string    `json:"gets_work,omitempty"`
	WebhookURL   string    `json:"webhook_url,omitempty"`
	LastDelivery *delivery `json:"last_delivery,omitempty"`
}

func (r *Runner) showAgent(actor string, p config.Persona, notes []string) agentJSON {
	title, pricing := r.modelAbout(p.Model)
	a := agentJSON{Nick: p.Nick, Name: p.Name, Role: p.Role, Team: p.Team, ReportsTo: p.ReportsTo, Model: p.Model,
		ModelTitle: title, ModelPricing: pricing,
		Effort: p.Effort, Budget: p.DailyBudgetUSD, Channels: p.Channels, Platform: p.Platform, Notes: notes}
	if a.Platform == "" {
		a.Platform = "chat"
	}
	if p.Platform == PlatformOutside {
		connected := r.outside.connected(p.Nick, r.now())
		a.Connected = &connected
		if seen := r.outside.lastSeen(p.Nick); !seen.IsZero() {
			a.LastSeen = &seen
		}
		today := r.outside.wakesOn(p.Nick, r.today())
		a.DailyWakes, a.WakesToday = wakeCap(p), &today
		a.GetsWork = "inbox"
		if url, _, _ := r.store.AgentWebhook(context.Background(), p.Nick); url != "" {
			a.GetsWork, a.WebhookURL = "webhook", url
			if d, ok := r.outside.lastDelivery(p.Nick); ok {
				a.LastDelivery = &d
			}
		}
	}
	if r.can(actor, canHire, onPerson(p.Nick)) {
		a.Prompt = &p.Prompt
	}
	return a
}

// agentChange is a request to hire or change an agent. Only what's given
// changes.
type agentChange struct {
	Nick      string   `json:"nick"`
	Name      *string  `json:"name"`
	Role      *string  `json:"role"`
	Team      *string  `json:"team"`
	ReportsTo *string  `json:"reports_to"`
	Model     *string  `json:"model"`
	Effort    *string  `json:"effort"`
	Budget    *float64 `json:"budget"`
	Channels  []string `json:"channels"`
	Prompt    *string  `json:"prompt"`
	Platform  *string  `json:"platform"`
	Preset    *string  `json:"preset"` // a role from the catalogue to start from
	// DailyWakes: how many times a day an outside agent may be woken.
	DailyWakes *int `json:"daily_wakes"`
}

// fields is the change as GuildServ fields, so the same checks apply.
func (c agentChange) fields() []field {
	var out []field
	add := func(name string, v *string) {
		if v != nil {
			out = append(out, field{name, *v})
		}
	}
	add("PRESET", c.Preset)
	add("NAME", c.Name)
	add("ROLE", c.Role)
	add("TEAM", c.Team)
	add("REPORTS-TO", c.ReportsTo)
	add("MODEL", c.Model)
	add("EFFORT", c.Effort)
	if c.Budget != nil {
		out = append(out, field{"BUDGET", strconv.FormatFloat(*c.Budget, 'f', -1, 64)})
	}
	if c.Channels != nil {
		out = append(out, field{"CHANNELS", strings.Join(c.Channels, ",")})
	}
	add("PROMPT", c.Prompt)
	add("PLATFORM", c.Platform)
	if c.DailyWakes != nil {
		out = append(out, field{"WAKES", strconv.Itoa(*c.DailyWakes)})
	}
	return out
}

func (r *Runner) apiOrg(w http.ResponseWriter, req *http.Request) {
	type personJSON struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"` // human, guest, or agent
		Owner     bool     `json:"owner,omitempty"`
		Role      string   `json:"role"`
		Team      string   `json:"team"`
		ReportsTo string   `json:"reports_to"`
		Access    []string `json:"access,omitempty"` // levels they've been given
	}
	var people []personJSON
	for _, p := range r.people() {
		kind := "human"
		switch {
		case p.agent:
			kind = "agent"
		case p.guest:
			kind = "guest"
		}
		people = append(people, personJSON{Name: p.name, Kind: kind, Owner: r.isOwner(p.name), Role: p.role, Team: p.team, ReportsTo: p.reportsTo, Access: p.access})
	}
	var teams []teamJSON
	all, _ := r.store.Teams(req.Context())
	for _, l := range all {
		if !r.seesTeam(actorOf(req), l.Name) {
			// A private team (or one inside it): that it's there, no more.
			teams = append(teams, teamJSON{Name: l.Name, Channel: l.Channel, Leads: []string{}, Parent: l.Parent, Private: true})
			continue
		}
		teams = append(teams, showTeam(l))
	}
	apiJSON(w, http.StatusOK, map[string]any{"owner": r.cfg.Owner, "people": people, "teams": teams})
}

func (r *Runner) apiAgent(w http.ResponseWriter, req *http.Request) {
	p, err := r.store.Agent(req.Context(), req.PathValue("nick"))
	if err != nil {
		apiRefusal(w, r.storeRefusal(req.PathValue("nick"), err))
		return
	}
	apiJSON(w, http.StatusOK, r.showAgent(actorOf(req), p, nil))
}

func (r *Runner) apiHire(w http.ResponseWriter, req *http.Request) {
	var c agentChange
	if !decode(w, req, &c) {
		return
	}
	if c.Nick == "" {
		apiError(w, http.StatusBadRequest, "nick is required")
		return
	}
	p, notes, err := r.hireAgent(actorOf(req), c.Nick, c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusCreated, r.showAgent(actorOf(req), p, notes))
}

func (r *Runner) apiSet(w http.ResponseWriter, req *http.Request) {
	var c agentChange
	if !decode(w, req, &c) {
		return
	}
	if c.Nick != "" {
		apiError(w, http.StatusBadRequest, "an agent's nick can't be changed; hire a new one")
		return
	}
	p, notes, err := r.setAgent(actorOf(req), req.PathValue("nick"), c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, r.showAgent(actorOf(req), p, notes))
}

func (r *Runner) apiFire(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, req, &body) {
		return
	}
	nick := req.PathValue("nick")
	if err := r.fireAgent(actorOf(req), nick, strings.TrimSpace(body.Reason)); err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]string{"fired": nick})
}

// approvalJSON is an approval as the API shows it.
type approvalJSON struct {
	ID       int64  `json:"id"`
	Agent    string `json:"agent"`
	Approver string `json:"approver"`
	Tool     string `json:"tool"`
	Summary  string `json:"summary"`
	// Input is the call's whole input, as JSON (none for an approval in the
	// agent's own words).
	Input     json.RawMessage `json:"input,omitempty"`
	Channel   string          `json:"channel"`
	Asker     string          `json:"asker"`
	Status    string          `json:"status"`
	DecidedBy string          `json:"decided_by,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	RaisedAt  time.Time       `json:"raised_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Route     []string        `json:"route,omitempty"`
	Next      string          `json:"next,omitempty"`    // who it moves to, if nobody decides
	MovesAt   time.Time       `json:"moves_at,omitzero"` // and when
}

func showApproval(a org.Approval) approvalJSON {
	j := approvalJSON{ID: a.ID, Agent: a.Agent, Approver: a.Approver, Tool: a.Tool, Summary: a.Summary, Channel: a.Channel,
		Asker: a.Asker, Status: string(a.Status), DecidedBy: a.DecidedBy, Reason: a.Reason, RaisedAt: a.RaisedAt, ExpiresAt: a.ExpiresAt,
		Route: a.Route}
	if json.Valid([]byte(a.Input)) {
		j.Input = json.RawMessage(a.Input)
	}
	return j
}

func (r *Runner) apiApprovals(w http.ResponseWriter, req *http.Request) {
	out := []approvalJSON{}
	for _, a := range r.approvalsFor(actorOf(req)) {
		j := showApproval(a)
		if i := slices.IndexFunc(a.Route, func(x string) bool { return strings.EqualFold(x, a.Approver) }); i >= 0 && i+1 < len(a.Route) && !a.MovesAt.IsZero() {
			j.Next, j.MovesAt = a.Route[i+1], a.MovesAt
		}
		out = append(out, j)
	}
	after := "off"
	if d := r.moveAfter(); d > 0 {
		after = shortDuration(d)
	}
	apiJSON(w, http.StatusOK, map[string]any{"approvals": out, "move_after": after})
}

func (r *Runner) apiDecide(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	decision := req.PathValue("decision")
	if err != nil || (decision != "approve" && decision != "deny") {
		apiError(w, http.StatusNotFound, "no such approval action")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, req, &body) {
		return
	}
	a, err := r.decideApproval(actorOf(req), id, decision == "approve", strings.TrimSpace(body.Reason))
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, showApproval(a))
}

// teamJSON is a team as the API shows it.
type teamJSON struct {
	Name          string   `json:"name"`
	Channel       string   `json:"channel"`
	Leads         []string `json:"leads"`
	Parent        string   `json:"parent,omitempty"`
	Focus         string   `json:"focus,omitempty"`
	Repos         []string `json:"repos,omitempty"`
	Connectors    []string `json:"connectors,omitempty"`
	Chatter       string   `json:"chatter,omitempty"`
	Private       bool     `json:"private"`
	MonthlyBudget float64  `json:"monthly_budget"`
}

func showTeam(l org.Team) teamJSON {
	leads := l.Leads
	if leads == nil {
		leads = []string{}
	}
	return teamJSON{Name: l.Name, Channel: l.Channel, Leads: leads, Parent: l.Parent, Focus: l.Focus, Repos: l.Repos, Connectors: l.Connectors, Chatter: l.Chatter, Private: l.Private, MonthlyBudget: l.MonthlyBudget}
}

// teamChange is a request to create or change a team.
type teamChange struct {
	Name   string   `json:"name"`
	Leads  []string `json:"leads"` // nil leaves them; [] means none
	Parent *string  `json:"parent"`
	Focus  *string  `json:"focus"`
	Repos  []string `json:"repos"` // nil leaves them; [] means none
	// Connectors, by ID or title: nil leaves them; [] means none.
	Connectors []string `json:"connectors"`
	Chatter    *string  `json:"chatter"` // quiet, summary, steps; "" for the default
	Channel    *string  `json:"channel"`
	Budget     *float64 `json:"budget"`
	Private    *bool    `json:"private"`
}

func (c teamChange) fields() []field {
	var out []field
	if c.Parent != nil {
		out = append(out, field{"PARENT", *c.Parent})
	}
	if c.Focus != nil {
		out = append(out, field{"FOCUS", *c.Focus})
	}
	if c.Repos != nil {
		repos := strings.Join(c.Repos, ",")
		if repos == "" {
			repos = "none"
		}
		out = append(out, field{"REPOS", repos})
	}
	if c.Connectors != nil {
		conns := strings.Join(c.Connectors, ",")
		if conns == "" {
			conns = "none"
		}
		out = append(out, field{"CONNECTORS", conns})
	}
	if c.Chatter != nil {
		out = append(out, field{"CHATTER", *c.Chatter})
	}
	if c.Leads != nil {
		leads := strings.Join(c.Leads, ",")
		if leads == "" {
			leads = "none"
		}
		out = append(out, field{"LEADS", leads})
	}
	if c.Channel != nil {
		out = append(out, field{"CHANNEL", *c.Channel})
	}
	if c.Budget != nil {
		out = append(out, field{"BUDGET", strconv.FormatFloat(*c.Budget, 'f', -1, 64)})
	}
	if c.Private != nil {
		out = append(out, field{"PRIVATE", strconv.FormatBool(*c.Private)})
	}
	return out
}

func (r *Runner) apiCreateTeam(w http.ResponseWriter, req *http.Request) {
	var c teamChange
	if !decode(w, req, &c) {
		return
	}
	l, err := r.createTeam(actorOf(req), c.Name, c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusCreated, showTeam(l))
}

func (r *Runner) apiSetTeam(w http.ResponseWriter, req *http.Request) {
	var c teamChange
	if !decode(w, req, &c) {
		return
	}
	if c.Name != "" {
		apiError(w, http.StatusBadRequest, "a team's name can't be changed")
		return
	}
	l, err := r.setTeam(actorOf(req), req.PathValue("name"), c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, showTeam(l))
}

func (r *Runner) apiBudget(w http.ResponseWriter, req *http.Request) {
	b, err := r.budgetFor(actorOf(req), req.PathValue("name"))
	if err != nil {
		apiRefusal(w, err)
		return
	}
	type line struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"` // human, agent, or team (one inside)
		Monthly *float64 `json:"monthly,omitempty"`
	}
	lines := []line{}
	for _, c := range b.commitments {
		l := line{Name: c.name, Kind: "human"}
		switch {
		case c.agent:
			l.Kind = "agent"
		case c.team:
			l.Kind = "team"
		}
		if c.agent || c.team || b.showCosts {
			l.Monthly = &c.monthly
		}
		lines = append(lines, l)
	}
	out := map[string]any{"team": b.team.Name, "monthly_budget": b.team.MonthlyBudget, "commitments": lines}
	if b.showCosts { // totals would give people's costs away
		out["committed"] = b.committed
		out["remaining"] = b.team.MonthlyBudget - b.committed
	}
	apiJSON(w, http.StatusOK, out)
}

// memberJSON is a human member as the API shows it. What they cost is only
// shown to the owner and their team's lead.
type memberJSON struct {
	Account     string   `json:"account"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Team        string   `json:"team"`
	ReportsTo   string   `json:"reports_to"`
	Guest       bool     `json:"guest"`
	MonthlyCost *float64 `json:"monthly_cost,omitempty"`
}

func (r *Runner) showMember(actor string, m org.Member) memberJSON {
	out := memberJSON{Account: m.Account, Name: m.Name, Role: m.Role, Team: m.Team, ReportsTo: m.ReportsTo, Guest: m.Guest}
	if r.can(actor, canSpend, onPerson(m.Account)) {
		out.MonthlyCost = &m.MonthlyCost
	}
	return out
}

// memberChange is a request to add or change a member.
type memberChange struct {
	Account   string   `json:"account"`
	Name      *string  `json:"name"`
	Role      *string  `json:"role"`
	Team      *string  `json:"team"`
	ReportsTo *string  `json:"reports_to"`
	Guest     *bool    `json:"guest"`
	Cost      *float64 `json:"cost"`
}

func (c memberChange) fields() []field {
	var out []field
	add := func(name string, v *string) {
		if v != nil {
			out = append(out, field{name, *v})
		}
	}
	add("NAME", c.Name)
	add("ROLE", c.Role)
	add("TEAM", c.Team)
	add("REPORTS-TO", c.ReportsTo)
	if c.Guest != nil {
		out = append(out, field{"GUEST", strconv.FormatBool(*c.Guest)})
	}
	if c.Cost != nil {
		out = append(out, field{"COST", strconv.FormatFloat(*c.Cost, 'f', -1, 64)})
	}
	return out
}

func (r *Runner) apiMember(w http.ResponseWriter, req *http.Request) {
	m, err := r.store.Member(req.Context(), req.PathValue("account"))
	if err != nil {
		apiRefusal(w, r.storeRefusal(req.PathValue("account"), err))
		return
	}
	apiJSON(w, http.StatusOK, r.showMember(actorOf(req), m))
}

func (r *Runner) apiAddMember(w http.ResponseWriter, req *http.Request) {
	var c memberChange
	if !decode(w, req, &c) {
		return
	}
	if c.Account == "" {
		apiError(w, http.StatusBadRequest, "account is required")
		return
	}
	m, err := r.addMember(actorOf(req), c.Account, c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusCreated, r.showMember(actorOf(req), m))
}

func (r *Runner) apiSetMember(w http.ResponseWriter, req *http.Request) {
	var c memberChange
	if !decode(w, req, &c) {
		return
	}
	if c.Account != "" {
		apiError(w, http.StatusBadRequest, "an account can't be renamed")
		return
	}
	m, err := r.setMember(actorOf(req), req.PathValue("account"), c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, r.showMember(actorOf(req), m))
}

func (r *Runner) apiRemoveMember(w http.ResponseWriter, req *http.Request) {
	account := req.PathValue("account")
	if err := r.removeMember(actorOf(req), account); err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]string{"removed": account})
}

func (r *Runner) apiPromote(w http.ResponseWriter, req *http.Request) {
	change := req.PathValue("change")
	if change == "disconnect" {
		if err := r.endIRCSessions(actorOf(req), req.PathValue("name")); err != nil {
			apiRefusal(w, err)
			return
		}
		apiJSON(w, http.StatusOK, map[string]string{"disconnected": req.PathValue("name")})
		return
	}
	if change != "promote" && change != "demote" {
		apiError(w, http.StatusNotFound, "no such change")
		return
	}
	var body struct {
		Team string `json:"team"` // their own team if not said
	}
	if !decode(w, req, &body) {
		return
	}
	l, movedTo, err := r.promotePerson(actorOf(req), req.PathValue("name"), strings.TrimSpace(body.Team), change == "promote")
	if err != nil {
		apiRefusal(w, err)
		return
	}
	out := map[string]any{"team": showTeam(l)}
	if movedTo != "" {
		out["reports_to"] = movedTo
	}
	apiJSON(w, http.StatusOK, out)
}

// What someone may do, access levels, and the org's log.

// apiMe is what the person acting may do: for each ability, whether across
// the whole org, and in which teams and for whom. The console uses it to
// offer only what will work; the runner still checks every change.
func (r *Runner) apiMe(w http.ResponseWriter, req *http.Request) {
	actor := actorOf(req)
	type reach struct {
		Org    bool     `json:"org"`
		Teams  []string `json:"teams"`
		People []string `json:"people"`
	}
	teams, _ := r.store.Teams(req.Context())
	people := r.people()
	out := map[string]*reach{}
	for _, ab := range abilities {
		x := &reach{Org: r.can(actor, ab, wholeOrg), Teams: []string{}, People: []string{}}
		for _, t := range teams {
			if r.can(actor, ab, inTeam(t.Name)) {
				x.Teams = append(x.Teams, t.Name)
			}
		}
		for _, p := range people {
			if r.can(actor, ab, onPerson(p.name)) {
				x.People = append(x.People, p.name)
			}
		}
		out[string(ab)] = x
	}
	access := []string{}
	for _, l := range r.held(actor) {
		access = append(access, l.Name)
	}
	if r.isOwner(actor) {
		access = []string{"Owner"} // everything, whatever the levels say
	}
	apiJSON(w, http.StatusOK, map[string]any{"account": actor, "owner": r.isOwner(actor), "guest": r.isGuest(actor), "access": access, "abilities": out})
}

// accessJSON is an access level as the API shows it.
type accessJSON struct {
	Name      string   `json:"name"`
	Abilities []string `json:"abilities"`
	Scope     string   `json:"scope"`
	Teams     []string `json:"teams,omitempty"`
	Preset    bool     `json:"preset"`
	Describe  string   `json:"describe"`
}

func showAccess(l org.AccessLevel) accessJSON {
	abs := l.Abilities
	if abs == nil {
		abs = []string{}
	}
	return accessJSON{Name: l.Name, Abilities: abs, Scope: l.Scope, Teams: l.Teams, Preset: isPreset(l.Name), Describe: describeLevel(l)}
}

func (r *Runner) apiAccessLevels(w http.ResponseWriter, req *http.Request) {
	out := []accessJSON{}
	for _, l := range r.accessLevels() {
		out = append(out, showAccess(l))
	}
	apiJSON(w, http.StatusOK, map[string]any{"levels": out, "abilities": abilities, "scopes": scopes})
}

func (r *Runner) apiSetAccessLevel(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Abilities []string `json:"abilities"`
		Scope     string   `json:"scope"`
		Teams     []string `json:"teams"`
	}
	if !decode(w, req, &body) {
		return
	}
	l, err := r.setAccessLevel(actorOf(req), req.PathValue("name"), body.Abilities, body.Scope, body.Teams)
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, showAccess(l))
}

func (r *Runner) apiDropAccessLevel(w http.ResponseWriter, req *http.Request) {
	if err := r.dropAccessLevel(actorOf(req), req.PathValue("name")); err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]string{"dropped": req.PathValue("name")})
}

func (r *Runner) apiGiveAccess(w http.ResponseWriter, req *http.Request) {
	m, err := r.giveAccess(actorOf(req), req.PathValue("name"), req.PathValue("level"), req.Method == "POST")
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, r.showMember(actorOf(req), m))
}

func (r *Runner) apiLog(w http.ResponseWriter, req *http.Request) {
	limit := 100
	if n, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	type entryJSON struct {
		ID     int64     `json:"id"`
		At     time.Time `json:"at"`
		Actor  string    `json:"actor"`
		Action string    `json:"action"`
		Target string    `json:"target"`
		Team   string    `json:"team,omitempty"`
		Detail string    `json:"detail,omitempty"`
	}
	out := []entryJSON{}
	for _, e := range r.logFor(actorOf(req), limit) {
		out = append(out, entryJSON{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action, Target: e.Target, Team: e.Team, Detail: e.Detail})
	}
	apiJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// The company's description and goals, and teams' objectives.

type goalJSON struct {
	ID      int64  `json:"id"`
	Team    string `json:"team,omitempty"`
	Text    string `json:"text"`
	Measure string `json:"measure,omitempty"`
	Target  string `json:"target,omitempty"`
	Current string `json:"current,omitempty"`
	Serves  int64  `json:"serves,omitempty"`
}

func showGoal(g org.Goal) goalJSON {
	return goalJSON{ID: g.ID, Team: g.Team, Text: g.Text, Measure: g.Measure, Target: g.Target, Current: g.Current, Serves: g.Serves}
}

// goalChange is a request to add or change a goal. Only what's given changes.
type goalChange struct {
	Team    *string `json:"team"`
	Text    *string `json:"text"`
	Measure *string `json:"measure"`
	Target  *string `json:"target"`
	Current *string `json:"current"`
	Serves  *int64  `json:"serves"`
}

func (c goalChange) fields() []field {
	var out []field
	add := func(name string, v *string) {
		if v != nil {
			out = append(out, field{name, *v})
		}
	}
	add("TEXT", c.Text)
	add("TEAM", c.Team)
	add("MEASURE", c.Measure)
	add("TARGET", c.Target)
	add("CURRENT", c.Current)
	if c.Serves != nil {
		out = append(out, field{"SERVES", strconv.FormatInt(*c.Serves, 10)})
	}
	return out
}

func (r *Runner) apiCompany(w http.ResponseWriter, req *http.Request) {
	d, _ := r.store.Company(req.Context())
	goals, _ := r.store.Goals(req.Context())
	out := []goalJSON{}
	for _, g := range goals {
		if r.seesTeam(actorOf(req), g.Team) {
			out = append(out, showGoal(g))
		}
	}
	apiJSON(w, http.StatusOK, map[string]any{"description": d, "goals": out, "guests_ask": r.guestsAsk(req.Context())})
}

// apiSetCompany changes what's given: the description, and whether guests
// may have agents work for them (guests_ask, the owner's call).
func (r *Runner) apiSetCompany(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Description *string `json:"description"`
		GuestsAsk   *bool   `json:"guests_ask"`
	}
	if !decode(w, req, &body) {
		return
	}
	if body.GuestsAsk != nil {
		if err := r.setGuestsAsk(actorOf(req), *body.GuestsAsk); err != nil {
			apiRefusal(w, err)
			return
		}
	}
	if body.Description != nil {
		if err := r.setCompany(actorOf(req), *body.Description); err != nil {
			apiRefusal(w, err)
			return
		}
	}
	d, _ := r.store.Company(req.Context())
	apiJSON(w, http.StatusOK, map[string]any{"description": d, "guests_ask": r.guestsAsk(req.Context())})
}

func (r *Runner) apiAddGoal(w http.ResponseWriter, req *http.Request) {
	var c goalChange
	if !decode(w, req, &c) {
		return
	}
	g, err := r.addGoal(actorOf(req), c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusCreated, showGoal(g))
}

func goalIDOf(w http.ResponseWriter, req *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such goal")
		return 0, false
	}
	return id, true
}

func (r *Runner) apiSetGoal(w http.ResponseWriter, req *http.Request) {
	id, ok := goalIDOf(w, req)
	if !ok {
		return
	}
	var c goalChange
	if !decode(w, req, &c) {
		return
	}
	g, err := r.setGoal(actorOf(req), id, c.fields())
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, showGoal(g))
}

func (r *Runner) apiDropGoal(w http.ResponseWriter, req *http.Request) {
	id, ok := goalIDOf(w, req)
	if !ok {
		return
	}
	if err := r.dropGoal(actorOf(req), id); err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]any{"dropped": id})
}

// apiSetMoveAfter sets how long an approval waits on one person before it
// moves on ({"after": "4h"}, or "off"); the owner only.
func (r *Runner) apiSetMoveAfter(w http.ResponseWriter, req *http.Request) {
	var body struct {
		After string `json:"after"`
	}
	if !decode(w, req, &body) {
		return
	}
	d, err := r.setMoveAfter(actorOf(req), body.After)
	if err != nil {
		apiRefusal(w, err)
		return
	}
	after := "off"
	if d > 0 {
		after = shortDuration(d)
	}
	apiJSON(w, http.StatusOK, map[string]string{"move_after": after})
}
