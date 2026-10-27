// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/netspec"
	"github.com/guildnet-chat/guildnet/templates"
)

// The home page (the base domain) for people signed in with Google: their
// networks, and creating one. Each network's console stays on its own
// address, signed in to with that network's IRC accounts.

type homeKeyT int

const homeKey homeKeyT = 0

// homeSession is someone signed in on the home page.
type homeSession struct {
	sessionWithToken
	person Person
}

func homeOf(r *http.Request) homeSession {
	h, _ := r.Context().Value(homeKey).(homeSession)
	return h
}

func (p *Platform) startHomeSession(w http.ResponseWriter, r *http.Request, person Person) error {
	token := newToken()
	s := Session{Account: person.ID, CSRF: newToken(), Expires: p.opts.Now().Add(sessionTTL)}
	if err := p.opts.Sessions.Save(r.Context(), hashToken(token), s); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	return nil
}

// loadHomeSession finds the home session a request's cookie belongs to. A
// network's console session is no good here.
func (p *Platform) loadHomeSession(r *http.Request) (homeSession, bool) {
	c, err := r.Cookie(p.cookieName())
	if err != nil || c.Value == "" || p.opts.People == nil {
		return homeSession{}, false
	}
	hash := hashToken(c.Value)
	s, err := p.opts.Sessions.Get(r.Context(), hash)
	if err != nil || s.Network != "" || !p.opts.Now().Before(s.Expires) {
		return homeSession{}, false
	}
	person, err := p.opts.People.Person(r.Context(), s.Account)
	if err != nil || person.Blocked {
		return homeSession{}, false
	}
	p.noteSeen(r.Context(), person.ID)
	return homeSession{sessionWithToken: sessionWithToken{Session: s, hash: hash}, person: person}, true
}

// lastSeen is when each person was last noted as signed in, so that's
// written at most hourly, not on every page.
type lastSeen struct {
	mu sync.Mutex
	at map[string]time.Time
}

const seenEvery = time.Hour

func (p *Platform) noteSeen(ctx context.Context, id string) {
	if p.opts.People == nil {
		return
	}
	now := p.opts.Now()
	p.seen.mu.Lock()
	if p.seen.at == nil {
		p.seen.at = map[string]time.Time{}
	}
	last, ok := p.seen.at[id]
	due := !ok || now.Sub(last) >= seenEvery
	if due {
		p.seen.at[id] = now
	}
	p.seen.mu.Unlock()
	if due {
		if err := p.opts.People.Seen(ctx, id, now); err != nil {
			monitor.Problem(p.opts.Logger, "noting %s was seen: %v", id, err)
		}
	}
}

// blockedMessage is what someone the admins blocked is told when they try
// to sign in.
func (p *Platform) blockedMessage() string {
	return "This account can't sign in. If you think that's a mistake, write to " + p.address("support") + "."
}

// address is one of the brand's addresses: privacy (the privacy policy),
// support (help, billing, refunds), security, or hello (anything else).
func (p *Platform) address(local string) string {
	b := p.brand()
	switch local {
	case "privacy":
		return b.Privacy
	case "support":
		return b.Support
	case "security":
		return b.Security
	default:
		return b.Hello
	}
}

// person serves h only to someone signed in on the home page; changes must
// carry their CSRF token.
func (p *Platform) person(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := p.loadHomeSession(r)
		if !ok {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		// Two-step first, where it's required: nothing else until it's set up.
		if r.URL.Path != "/account/two-step" && r.URL.Path != "/signout" && p.homeSetUpFirst(r.Context(), s.person) {
			http.Redirect(w, r, "/account/two-step", http.StatusSeeOther)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" &&
			subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
			http.Error(w, "That form has expired. Go back, reload the page, and try again.", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), homeKey, s)))
	}
}

// homePage is what every home template gets.
type homePage struct {
	Email, CSRF, Error string
	Done               string // "Done.", once, after a form went through
	Admin              bool   // they're one of GuildNet's admins
	Base               string // the base domain, as people see it
	HtmxIntegrity      string
	Site               site
	Data               any
}

// site is what guildnet.chat's own header and footer show, on every page
// of it.
type site struct {
	SignedIn bool   // signed in on guildnet.chat
	SignIn   bool   // there's a way to sign in here
	Admin    bool   // one of GuildNet's admins
	Email    string // who's signed in, on pages that offer signing out
	CSRF     string // for signing out, on those pages
	Front    bool   // the front page, to a visitor (where an extension's site may call them in)
	Hello    string // GuildNet's address for anything
}

func (p *Platform) siteOf(r *http.Request) site {
	st := site{SignIn: p.canSignIn(), Hello: p.address("hello")}
	if s, ok := p.loadHomeSession(r); ok {
		st.SignedIn, st.Admin = true, p.admits(s.person)
	}
	st.Front = r.URL.Path == "/" && !st.SignedIn
	return st
}

// canSignIn says whether there's a way to sign in on the home page.
func (p *Platform) canSignIn() bool {
	return p.opts.Google != nil || p.opts.DevSignIn || p.homeSigninView().Email
}

func (p *Platform) renderHome(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	pg := homePage{Base: r.Host, HtmxIntegrity: htmxIntegrity, Data: data, Site: p.siteOf(r)}
	if s, ok := p.loadHomeSession(r); ok {
		pg.Email, pg.CSRF, pg.Admin = s.person.Email, s.CSRF, p.admits(s.person)
		pg.Site.Email, pg.Site.CSRF = pg.Email, pg.CSRF
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

// frontDoor is the home page's own front door: your networks, once you've
// signed in.
func (p *Platform) frontDoor(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.loadHomeSession(r); ok {
		http.Redirect(w, r, "/networks", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

func (p *Platform) homeHandler() http.Handler {
	home := http.NewServeMux()
	claimed := map[string]bool{}
	if p.hooks.Home != nil {
		p.hooks.Home(routes{p: p, mux: home, home: true, claimed: claimed})
	}
	switch {
	case p.opts.FirstRun != nil:
		// A self-hosted install: setup until it's done, then its network.
		home.HandleFunc("GET /{$}", p.setupPage)
		home.HandleFunc("POST /setup", p.firstRunSetUp)
		home.HandleFunc("POST /setup/open", p.openFirst)
	case !claimed["GET /{$}"]:
		home.HandleFunc("GET /{$}", p.frontDoor)
	}
	if !claimed["GET /robots.txt"] {
		home.HandleFunc("GET /robots.txt", consoleRobots)
	}
	home.HandleFunc("GET /signin", p.homeSigninPage)
	home.HandleFunc("GET /signin/google", p.googleStart)
	home.HandleFunc("GET /signin/google/callback", p.googleCallback)
	if p.opts.DevSignIn {
		home.HandleFunc("POST /signin/dev", p.devSignin)
	}
	if p.opts.HomeTwoSteps != nil {
		home.HandleFunc("GET /signin/two-step", p.homeStepPage)
		home.HandleFunc("POST /signin/two-step", p.homeStepDone)
		home.HandleFunc("GET /account/two-step", p.person(p.homeTwoStepPage))
		home.HandleFunc("POST /account/two-step", p.person(p.setUpHomeTwoStep))
	}
	if p.opts.Mail != nil && p.opts.People != nil {
		home.HandleFunc("POST /signin/email", p.askForSignInEmail)
		home.HandleFunc("GET /signin/email/{token}", p.useSignInLink)
		home.HandleFunc("POST /signin/email/code", p.enterSignInCode)
	}
	home.HandleFunc("POST /signout", p.person(p.homeSignout))
	home.HandleFunc("GET /networks", p.person(p.networksPage))
	home.HandleFunc("GET /networks/new", p.person(p.newNetworkPage))
	home.HandleFunc("POST /networks", p.person(p.createNetwork))
	home.HandleFunc("GET /networks/{name}", p.person(p.networkPage))
	home.HandleFunc("POST /networks/{name}/retry", p.person(p.retryNetwork))
	home.HandleFunc("POST /networks/{name}/open", p.person(p.openNetwork))
	home.HandleFunc("POST /networks/{name}/remove", p.person(p.removeFromHome))
	home.HandleFunc("POST /networks/{name}/password", p.person(p.setOwnerPassword))
	home.HandleFunc("POST /networks/{name}/two-step/reset", p.person(p.resetOwnersTwoStep))
	home.HandleFunc("POST /networks/{name}/github", p.person(p.connectGitHub))
	home.HandleFunc("POST /networks/{name}/github/disconnect", p.person(p.disconnectGitHub))
	home.HandleFunc("GET /github/callback", p.githubCallback)
	home.HandleFunc("POST /networks/{name}/connectors/{connector}", p.person(p.connectService))
	home.HandleFunc("POST /networks/{name}/connectors/{connector}/disconnect", p.person(p.disconnectService))
	home.HandleFunc("GET /oauth/callback", p.oauthCallback)
	home.HandleFunc("GET /oauth/client-metadata.json", p.clientMetadata)
	home.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.renderHome(w, r, http.StatusNotFound, "home-not-found", nil)
	})
	return home
}

type homeSignin struct {
	Google, Dev, Email bool
}

func (p *Platform) homeSigninPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.loadHomeSession(r); ok {
		http.Redirect(w, r, "/networks", http.StatusSeeOther)
		return
	}
	p.renderHome(w, r, http.StatusOK, "home-signin", withError{View: p.homeSigninView()})
}

// devSignin signs in by email alone: the stand-in for Google on a
// developer's own machine, never on the internet (see Options.DevSignIn).
func (p *Platform) devSignin(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	if !mail.Plausible(email) {
		p.renderHome(w, r, http.StatusBadRequest, "home-signin", withError{Err: "That doesn't look like an email address.", View: homeSignin{Dev: true}})
		return
	}
	person, err := p.opts.People.EmailPerson(r.Context(), email)
	if err == nil && person.Blocked {
		p.renderHome(w, r, http.StatusForbidden, "home-signin", withError{Err: p.blockedMessage(), View: homeSignin{Dev: true}})
		return
	}
	if err == nil {
		err = p.signInHome(w, r, person, "/networks")
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "dev sign-in: %v", err)
		http.Error(w, "Couldn't sign you in.", http.StatusInternalServerError)
	}
}

func (p *Platform) homeSignout(w http.ResponseWriter, r *http.Request) {
	p.opts.Sessions.Delete(r.Context(), homeOf(r).hash)
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Your networks.

// setupTimeout is how long setting up may take before it counts as stuck
// (say, the platform restarted part way).
const setupTimeout = 10 * time.Minute

type networkView struct {
	Network
	Address string // where its console is
	Stuck   bool   // setting up (or deleting) stopped part way
	IRC     ircSettings
	// GitHub: whether the public app is set up here, and what the network's
	// connected to.
	GitHubOn bool
	GitHub   GitHubLink
	// Connectors are the services it can be connected to, and whether it is.
	Connectors []connectorView
	// TwoStep: whether the owner signs in to its console in two steps.
	TwoStep bool
}

func (v networkView) Failed() bool { return v.Status == Failed || v.Stuck }

// DeleteFailed says whether what didn't finish was deleting it.
func (v networkView) DeleteFailed() bool {
	return v.Problem == deleteProblem || (v.Stuck && v.Status == Deleting)
}

func (p *Platform) view(r *http.Request, n Network) networkView {
	v := networkView{Network: n, Address: p.networkOrigin(r, n.Name), IRC: p.ircSettings(n.Name, n.OwnerNick)}
	v.Stuck = (n.Status == Creating || n.Status == Deleting) && p.opts.Now().Sub(n.Updated) > setupTimeout
	if v.GitHubOn = p.githubApp != nil; v.GitHubOn {
		v.GitHub, _ = p.opts.Networks.GitHubOf(r.Context(), n.Name)
	}
	v.Connectors = p.connectorViews(r.Context(), n.Name)
	if p.opts.TwoSteps != nil && n.OwnerNick != "" {
		t, _ := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, n.OwnerNick)
		v.TwoStep = t.On()
	}
	return v
}

type networksView struct {
	Networks  []networkView
	CanCreate bool
	Open      bool          // creating networks is switched on here
	Why       string        // why they may not create one, if they may not
	Note      template.HTML // shown instead of the button to create one
}

// mayCreate says whether this person may create a network now, and if
// not, why.
func (p *Platform) mayCreate(r *http.Request) (networksView, error) {
	s := homeOf(r)
	// A self-hosted install runs only the network first-run setup makes.
	v := networksView{Open: p.opts.Host != nil && p.opts.FirstRun == nil}
	owned, err := p.opts.Networks.NetworksOwnedBy(r.Context(), s.person.ID)
	if err != nil {
		return v, err
	}
	for _, n := range owned {
		v.Networks = append(v.Networks, p.view(r, n))
	}
	rule := p.oneNetwork
	if p.hooks.MayCreate != nil {
		rule = p.hooks.MayCreate
	}
	c, err := rule(r.Context(), s.person, len(owned))
	if err != nil {
		return v, err
	}
	v.Why, v.Note = c.Why, c.Note
	v.CanCreate = v.Open && c.Why == ""
	return v, nil
}

// oneNetwork is the open edition's rule: one network here, made by whoever
// sets it up. (Networks set up by hand don't count.)
func (p *Platform) oneNetwork(ctx context.Context, _ Person, _ int) (CreateAnswer, error) {
	all, err := p.opts.Networks.AllNetworks(ctx)
	if err != nil {
		return CreateAnswer{}, err
	}
	for _, n := range all {
		if n.Owner != "" {
			return CreateAnswer{Why: "This " + p.brand().Name + " runs one network, and it has been set up."}, nil
		}
	}
	return CreateAnswer{}, nil
}

func (p *Platform) networksPage(w http.ResponseWriter, r *http.Request) {
	v, err := p.mayCreate(r)
	if err != nil {
		monitor.Problem(p.opts.Logger, "your networks: %v", err)
		http.Error(w, "Something went wrong. Try again in a moment.", http.StatusInternalServerError)
		return
	}
	p.renderHome(w, r, http.StatusOK, "home-networks", withError{View: v})
}

// Creating a network.

type newNetworkView struct {
	Templates []templates.Template
	Timezones []string
	Providers []providerKey
	Form      map[string]string
}

func (p *Platform) newNetworkForm(r *http.Request, form map[string]string) newNetworkView {
	if form == nil {
		form = map[string]string{"template": templates.All[0].Name, "provider": string(models.Anthropic)}
	}
	var providers []providerKey
	for _, prov := range models.Providers {
		providers = append(providers, providerKey{Provider: string(prov), Title: prov.Title()})
	}
	return newNetworkView{Templates: templates.All, Timezones: commonTimezones, Providers: providers, Form: form}
}

func (p *Platform) newNetworkPage(w http.ResponseWriter, r *http.Request) {
	v, err := p.mayCreate(r)
	if err != nil || !v.CanCreate {
		p.renderHome(w, r, http.StatusForbidden, "home-networks", withError{Err: cantCreate(v), View: v})
		return
	}
	p.renderHome(w, r, http.StatusOK, "home-new", withError{View: p.newNetworkForm(r, nil)})
}

func cantCreate(v networksView) string {
	if !v.Open {
		return "Creating networks isn't open here yet."
	}
	return v.Why
}

// reservedNames are addresses a network can't have: GuildNet's own, ones
// people would mistake for it, and names email uses in guildnet.chat's DNS
// (a record there would hide the network's address).
var reservedNames = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`www www2 api app apps admin administrator root mail email smtp imap pop ftp send bounce
		chat irc web docs doc help support status blog news dev staging test demo static assets cdn img media
		ns ns1 ns2 dns host hosts platform console guildnet guildserv hq about login signin signup auth oauth sso
		account accounts id beta billing pay invoice shop store git github security abuse postmaster hostmaster
		webmaster noreply no-reply official team teams`) {
		reservedNames[n] = true
	}
}

// reservedNicks are nicks an owner can't take on any network: its services
// and bots.
var reservedNicks = []string{"guildserv", "ledger", "nickserv", "chanserv", "hostserv", "histserv", "operserv", "global"}

// minPassword is the shortest IRC password anyone may choose.
const minPassword = 12

// passwordProblem says what's wrong with a new IRC password, in words for
// the person: Ergo takes no spaces, nor a colon first.
func passwordProblem(pw string) string {
	switch {
	case len(pw) < minPassword || len(pw) > 256:
		return fmt.Sprintf("Your IRC password must be at least %d characters.", minPassword)
	case strings.ContainsFunc(pw, unicode.IsSpace):
		return "IRC passwords can't have spaces in them. Use dashes or other characters instead."
	case strings.HasPrefix(pw, ":"):
		return "An IRC password can't start with a colon."
	}
	return ""
}

type networkForm struct {
	name, template, timezone, nick, password, provider, key string
}

// check says what's wrong with a create form, in words for the person.
func (f networkForm) check() string {
	switch {
	case !netspec.ValidName(f.name):
		return "A network's name is 2 to 30 lowercase letters, digits and single hyphens, starting with a letter."
	case reservedNames[f.name]:
		return "That name is reserved. Pick another."
	}
	return f.checkSettings()
}

// checkSettings says what's wrong with the rest of the form: all but the
// name (first-run setup's is the install's).
func (f networkForm) checkSettings() string {
	tpl, ok := templates.Find(f.template)
	if !ok {
		return "Pick one of the templates."
	}
	if loc, err := time.LoadLocation(f.timezone); err != nil || f.timezone == "" || f.timezone == "Local" || loc == nil {
		return "Pick your company's timezone, like America/Edmonton or Europe/Paris."
	}
	if !config.ValidNick(f.nick) {
		return "Your nick is letters, digits, - and _, starting with a letter, at most 32 characters."
	}
	lower := strings.ToLower(f.nick)
	if slices.Contains(reservedNicks, lower) || slices.Contains(tpl.Agents, lower) {
		return "That nick belongs to a bot or one of the template's agents. Pick another."
	}
	if msg := passwordProblem(f.password); msg != "" {
		return msg
	}
	prov, ok := providerNamed(f.provider)
	if !ok {
		return "Pick the model provider your key is for."
	}
	return keyProblem(prov, f.key)
}

func (p *Platform) createNetwork(w http.ResponseWriter, r *http.Request) {
	f := networkForm{
		name: strings.TrimSpace(r.PostFormValue("name")), template: r.PostFormValue("template"),
		timezone: strings.TrimSpace(r.PostFormValue("timezone")), nick: strings.TrimSpace(r.PostFormValue("nick")),
		password: r.PostFormValue("password"), provider: r.PostFormValue("provider"), key: strings.TrimSpace(r.PostFormValue("model_key")),
	}
	again := func(status int, msg string) {
		form := map[string]string{"name": f.name, "template": f.template, "timezone": f.timezone, "nick": f.nick, "provider": f.provider}
		p.renderHome(w, r, status, "home-new", withError{Err: msg, View: p.newNetworkForm(r, form)})
	}
	// One at a time, so nobody gets two networks by asking twice at once.
	p.creating.Lock()
	defer p.creating.Unlock()
	v, err := p.mayCreate(r)
	if err != nil || !v.CanCreate {
		p.renderHome(w, r, http.StatusForbidden, "home-networks", withError{Err: cantCreate(v), View: v})
		return
	}
	if msg := f.check(); msg != "" {
		again(http.StatusBadRequest, msg)
		return
	}
	prov, _ := providerNamed(f.provider)
	if msg := p.checkKey(r.Context(), prov, f.key); msg != "" {
		again(http.StatusBadRequest, msg)
		return
	}
	var keys ModelKeys
	keys.Set(prov, f.key)
	n := Network{
		Name: f.name, Region: p.opts.Region, Status: Creating, Owner: homeOf(r).person.ID, OwnerNick: f.nick,
		Template: f.template, Timezone: f.timezone, Keys: keys, Updated: p.opts.Now(),
		RunnerToken: newToken(), ErgoToken: newToken(), ChatKey: newToken(),
	}
	err = p.opts.Networks.AddNetwork(r.Context(), n)
	// A network deleted under that name may only be waiting for its desks to
	// go: try once more now.
	if errors.Is(err, ErrTaken) {
		if old, oerr := p.opts.Networks.Network(r.Context(), n.Name); oerr == nil && old.Status == Deleting && old.Problem == desksLeft &&
			p.forgetNetwork(r.Context(), old) == nil {
			err = p.opts.Networks.AddNetwork(r.Context(), n)
		}
	}
	if err != nil {
		if errors.Is(err, ErrTaken) {
			again(http.StatusBadRequest, "That name is taken. Pick another.")
			return
		}
		monitor.Problem(p.opts.Logger, "creating network %s: %v", n.Name, err)
		again(http.StatusInternalServerError, "Couldn't create it. Try again in a moment.")
		return
	}
	origin := p.networkOrigin(r, n.Name)
	p.background(func() { p.setUp(n.Name, f.password, origin) })
	http.Redirect(w, r, "/networks/"+n.Name, http.StatusSeeOther)
}

// networkOrigin is where a network's console and chat are: its name on the
// base domain, with the port this request came in on.
func (p *Platform) networkOrigin(r *http.Request, name string) string {
	origin := p.scheme() + "://" + name + "." + p.opts.BaseDomain
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		origin += ":" + port
	}
	return origin
}

// setUp has the host start the network, then makes the owner's IRC account
// with the password they chose (which goes nowhere else and isn't kept),
// and waits until the network answers.
func (p *Platform) setUp(name, password, origin string) {
	defer monitor.Recover()
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	fail := func(n Network, problem string, err error) {
		monitor.Problem(p.opts.Logger, "setting up %s: %v", name, err)
		n.Status, n.Problem, n.Updated = Failed, problem, p.opts.Now()
		if err := p.opts.Networks.UpdateNetwork(context.Background(), n); err != nil {
			monitor.Problem(p.opts.Logger, "setting up %s: %v", name, err)
		}
	}
	n, err := p.opts.Networks.Network(ctx, name)
	if err != nil {
		monitor.Problem(p.opts.Logger, "setting up %s: %v", name, err)
		return
	}
	addrs, err := p.opts.Host.Put(ctx, name, p.specFor(n, origin))
	if err != nil {
		fail(n, "Starting the network didn't work.", err)
		return
	}
	n.RunnerURL, n.ErgoURL, n.ChatURL, n.Updated = addrs.RunnerURL, addrs.ErgoURL, addrs.ChatURL, p.opts.Now()
	if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
		fail(n, "Starting the network didn't work.", err)
		return
	}
	if err := waitFor(ctx, func() error { _, err := ergoStatus(ctx, n); return err }); err != nil {
		fail(n, "The network's IRC server didn't start.", err)
		return
	}
	if err := p.registerPersonsAccount(ctx, n, n.OwnerNick, password); err != nil {
		problem := "Making your IRC account didn't work."
		if errors.Is(err, errAccountExists) {
			problem = "Your nick already has a different password on this network. Try again with that one."
		}
		fail(n, problem, err)
		return
	}
	if err := waitFor(ctx, func() error {
		return (runner{n: n, actor: n.OwnerNick}).call(ctx, "GET", "/v1/org", nil, nil)
	}); err != nil {
		fail(n, "The network's agents didn't start.", err)
		return
	}
	n.Status, n.Problem, n.Updated = Ready, "", p.opts.Now()
	if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
		monitor.Problem(p.opts.Logger, "setting up %s: %v", name, err)
		return
	}
	p.opts.Logger.Printf("network %s: ready", name)
	p.tellOwner(ctx, n, welcomeEmail(n, origin))
}

// waitFor tries until try works or ctx is done.
func waitFor(ctx context.Context, try func() error) error {
	for {
		err := try()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ownNetwork is the network named in the path, if it's this person's.
func (p *Platform) ownNetwork(w http.ResponseWriter, r *http.Request) (Network, bool) {
	n, err := p.opts.Networks.Network(r.Context(), r.PathValue("name"))
	if err != nil || n.Owner == "" || n.Owner != homeOf(r).person.ID {
		http.NotFound(w, r)
		return Network{}, false
	}
	return n, true
}

// ownNetworkToChange is ownNetwork for deleting it, setting it up again or
// connecting it to more: one the server's admins suspended is left as it is,
// for them.
func (p *Platform) ownNetworkToChange(w http.ResponseWriter, r *http.Request) (Network, bool) {
	n, ok := p.ownNetwork(w, r)
	if ok && n.Status == Suspended {
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, http.StatusForbidden, "home-network", withError{Err: n.Name + " is suspended, so it can't be changed from here.", View: p.view(r, n)})
		return Network{}, false
	}
	return n, ok
}

func (p *Platform) networkPage(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	p.letFormsReach(w, r, n)
	p.renderHome(w, r, http.StatusOK, "home-network", withError{View: p.view(r, n)})
}

// letFormsReach lets this page's forms end up at n's console: browsers hold
// a form's redirect to the page's form-action, and "Open" redirects there.
func (p *Platform) letFormsReach(w http.ResponseWriter, r *http.Request, n Network) {
	reach := "form-action 'self' " + p.networkOrigin(r, n.Name)
	if p.githubApp != nil {
		reach += " " + p.opts.GitHub.web() // "Connect GitHub" goes on to GitHub
	}
	w.Header().Set("Content-Security-Policy", csp(reach))
}

func (p *Platform) retryNetwork(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetworkToChange(w, r)
	if !ok {
		return
	}
	v := p.view(r, n)
	if !v.Failed() {
		http.Redirect(w, r, "/networks/"+n.Name, http.StatusSeeOther)
		return
	}
	password := r.PostFormValue("password")
	if len(password) < minPassword {
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, http.StatusBadRequest, "home-network", withError{Err: "Enter the IRC password you chose (at least 12 characters).", View: v})
		return
	}
	n.Status, n.Problem, n.Updated = Creating, "", p.opts.Now()
	if err := p.opts.Networks.UpdateNetwork(r.Context(), n); err != nil {
		monitor.Problem(p.opts.Logger, "retrying %s: %v", n.Name, err)
		http.Error(w, "Something went wrong. Try again in a moment.", http.StatusInternalServerError)
		return
	}
	origin := p.networkOrigin(r, n.Name)
	p.background(func() { p.setUp(n.Name, password, origin) })
	http.Redirect(w, r, "/networks/"+n.Name, http.StatusSeeOther)
}

// Handing the owner over to their network's console, signed in: a one-time
// code, good for a minute, on that network only.

const handoffTTL = time.Minute

type handoff struct {
	network, account string
	expires          time.Time
}

type handoffs struct {
	mu sync.Mutex
	m  map[string]handoff // by the code's hash
}

func (h *handoffs) add(code string, x handoff) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]handoff{}
	}
	for k, v := range h.m {
		if x.expires.Sub(v.expires) > handoffTTL {
			delete(h.m, k) // long gone
		}
	}
	h.m[hashToken(code)] = x
}

// peek is what a code is for, leaving it to be used.
func (h *handoffs) peek(code string) (handoff, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	x, ok := h.m[hashToken(code)]
	return x, ok
}

// take uses a code up.
func (h *handoffs) take(code string) (handoff, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	x, ok := h.m[hashToken(code)]
	delete(h.m, hashToken(code))
	return x, ok
}

func (p *Platform) openNetwork(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	// Asleep, it's woken on arriving.
	if n.Status != Ready && n.Status != Asleep {
		http.Redirect(w, r, "/networks/"+n.Name, http.StatusSeeOther)
		return
	}
	code := newToken()
	p.handoffs.add(code, handoff{network: n.Name, account: n.OwnerNick, expires: p.opts.Now().Add(handoffTTL)})
	http.Redirect(w, r, p.networkOrigin(r, n.Name)+"/welcome?code="+code, http.StatusSeeOther)
}

// welcome is where a handoff lands, on the network's own address: it asks
// before signing in, so a link someone else made, and sent, can't sign a
// browser in as them unawares (the button posts from this page only).
func (p *Platform) welcome(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	code := r.URL.Query().Get("code")
	x, ok := p.handoffs.peek(code)
	if !p.handoffGood(x, ok, n) {
		p.handoffGone(w, r)
		return
	}
	// Already signed in here as them: nothing more to ask.
	if s, ok := sessionOf(r); ok && strings.EqualFold(s.Account, x.account) {
		p.handoffs.take(code)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p.render(w, r, http.StatusOK, "welcome", withError{View: struct{ Account, Code string }{x.account, code}})
}

func (p *Platform) handoffGood(x handoff, ok bool, n Network) bool {
	return ok && strings.EqualFold(x.network, n.Name) && p.opts.Now().Before(x.expires)
}

func (p *Platform) handoffGone(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusBadRequest, "signin", withError{Err: "That link has expired. Sign in with your IRC account and password."})
}

// openWelcome is the welcome page's button: the handoff, used up.
func (p *Platform) openWelcome(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	x, ok := p.handoffs.take(r.PostFormValue("code"))
	if !p.handoffGood(x, ok, n) {
		p.handoffGone(w, r)
		return
	}
	if s, ok := sessionOf(r); ok && strings.EqualFold(s.Account, x.account) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// The home page vouches for who they are, in place of the password; a
	// second step still has to be taken here.
	if err := p.signInAs(w, r, n, x.account); err != nil {
		monitor.Problem(p.opts.Logger, "signing %s in: %v", x.account, err)
		http.Error(w, "Couldn't sign you in. Try again in a moment.", http.StatusInternalServerError)
	}
}

// commonTimezones are offered on the create page; any IANA name works.
var commonTimezones = []string{
	"UTC", "America/Vancouver", "America/Los_Angeles", "America/Edmonton", "America/Denver", "America/Phoenix",
	"America/Winnipeg", "America/Chicago", "America/Toronto", "America/New_York", "America/Halifax", "America/St_Johns",
	"America/Mexico_City", "America/Bogota", "America/Sao_Paulo", "America/Argentina/Buenos_Aires",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin", "Europe/Amsterdam",
	"Europe/Madrid", "Europe/Rome", "Europe/Stockholm", "Europe/Warsaw", "Europe/Athens", "Europe/Kyiv",
	"Europe/Istanbul", "Africa/Lagos", "Africa/Nairobi", "Africa/Johannesburg", "Asia/Dubai", "Asia/Kolkata",
	"Asia/Singapore", "Asia/Shanghai", "Asia/Tokyo", "Asia/Seoul", "Australia/Perth", "Australia/Sydney",
	"Pacific/Auckland",
}
