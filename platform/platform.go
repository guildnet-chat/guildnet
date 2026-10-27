// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/limit"
	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/netspec"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// rootIcons are GuildNet's icons at the addresses browsers and phones ask
// for them, with their types.
var rootIcons = map[string]string{"/favicon.ico": "image/x-icon", "/apple-touch-icon.png": "image/png"}

// The same type for the icon in /static/ on every machine; some systems'
// tables say image/vnd.microsoft.icon.
func init() { mime.AddExtensionType(".ico", "image/x-icon") }

// htmxIntegrity is htmx 4.0.0's published SRI hash (from its README), which
// the vendored file was checked against.
const htmxIntegrity = "sha384-BvJpBiO8Kh31EqtJe5DRIeWrHWnCGkwytKs9NKFi86Hhw96dEqdEMzZDeK9iEGTc"

// Options configure the platform.
type Options struct {
	// BaseDomain is what networks' addresses end in: acme.<BaseDomain>.
	BaseDomain string
	Networks   Networks
	Sessions   Sessions
	// SecureCookies marks cookies Secure (and uses the __Host- prefix). On
	// for anything served over HTTPS, which is everything but local tests.
	SecureCookies bool
	// TrustedProxies are the addresses of the edge in front (Caddy). From
	// them, and only them, X-Forwarded-For says who the visitor is.
	TrustedProxies []netip.Prefix
	// ChatDir holds the built chat app (GuildNet's gamja fork); "" means no
	// web chat.
	ChatDir string
	// ChatSource is where the chat app's source is published (it's AGPL, so
	// its users are offered it), a repository's web address; "" means the
	// chat shows no Source link. ChatCommit is the commit the build is from.
	ChatSource, ChatCommit string
	// Source is where the console's own source is published (GuildNet is
	// AGPL: whoever runs a changed copy for others offers them its source),
	// a web address every page links to; "" means no Source link.
	Source string
	// People keeps who signs in on the home page; Google is how (nil means
	// no Google sign-in). DevSignIn signs in by email alone, for a
	// developer's own machine: never with SecureCookies.
	People People
	Google *GoogleSignIn
	// GitHub is GuildNet's public GitHub App, for customers' networks; nil
	// when it isn't set up.
	GitHub    *GitHubApp
	DevSignIn bool
	// Connections keeps networks' connections to connectors (Linear,
	// Notion...); nil means none can be connected here. Connectors is what's
	// offered (nil means connectors.Catalogue), reached through
	// ConnectorEgress; ClientMetadataURL is GuildNet's client ID metadata
	// document, "" meaning register with each service instead.
	Connections       Connections
	Connectors        []connectors.Connector
	ConnectorEgress   egress.Options
	ClientMetadataURL string
	// GitRepos keeps networks' git repositories on any host (git_repos.go);
	// nil means none can be added here. They're reached as connectors are,
	// and also at GitPrivateHosts: names of git servers on a private network
	// (a self-hosted install's own).
	GitRepos        GitRepos
	GitPrivateHosts []string
	// DeskBlocked are hosts no network's desks may reach, whatever its
	// owner's list says, beside GuildNet's own (sandbox.DefaultBlocked): each
	// blocks the name and every name under it.
	DeskBlocked []string
	// Host runs new networks, in Region; nil means networks can't be
	// created here.
	Host   Host
	Region string
	// FirstRun is a self-hosted install's one network, which first-run
	// setup makes (firstrun.go); nil means there's no setup here.
	FirstRun *FirstRun
	// ModelProviders overrides where model providers are reached, to check
	// keys (tests); a provider not in it is reached at its own address.
	ModelProviders map[models.Provider]string
	// KeysByBroker says networks' runners fetch their model keys from this
	// platform's broker, so the host is never sent them.
	KeysByBroker bool
	// ErgoAsks says networks' IRC servers ask this console about people's
	// logins (at its broker): their passwords are kept here only. Without
	// it (no broker: a developer's machine), Ergo keeps a copy to check.
	ErgoAsks bool
	// Links keeps invitations and password resets; nil means neither.
	Links Links
	// TwoSteps keeps two-step sign-in to consoles; nil means it can't be
	// used. With RequireTwoStep, the owner and anyone who may change things
	// across the whole org must set it up before anything else.
	TwoSteps       TwoSteps
	RequireTwoStep bool
	// HomeTwoSteps keeps two-step sign-in to the home page; nil means it
	// can't be used. With RequireHomeTwoStep, everyone signing in there must
	// set it up before anything else.
	HomeTwoSteps       HomeTwoSteps
	RequireHomeTwoStep bool
	// Mail sends GuildNet's own email (beta invites, invitations into
	// networks); nil means none is sent, and people send links themselves.
	Mail mail.Sender
	// Alerts keeps where people want email alerts; nil means none but the
	// owner's.
	Alerts Alerts
	// Onboarding keeps the getting-started checklist; nil means there's
	// none.
	Onboarding Onboarding
	// IRCPort is where IRC apps connect, with TLS (6697 at the edge); 0
	// means IRC apps can't connect here.
	IRCPort int
	// Sandbox is where desks of GuildNet's own are; its zero value means
	// there are none.
	Sandbox Sandbox
	// Extension adds what the open console doesn't have (extension.go);
	// nil means none.
	Extension Extension
	// Brand is what this GuildNet goes by (brand.go); its zero value is
	// GuildNet.
	Brand  Brand
	Logger *log.Logger
	Now    func() time.Time // for tests; nil means time.Now
}

// Host is the machine's host agent, which runs networks.
type Host interface {
	Put(ctx context.Context, name string, s netspec.Spec) (netspec.Addresses, error)
	Remove(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Stats(ctx context.Context) (netspec.Stats, error)
}

// Platform is the web app.
type Platform struct {
	opts  Options
	pages map[string]*template.Template
	email *template.Template // GuildNet's emails, in HTML
	// sectionPages are the extension's sections of pages (Pages.Sections).
	sectionPages *template.Template
	static       func(name string) (string, error) // a static file's versioned address
	staticFiles  layers                            // the console's static files, and the extension's
	// codeAttempts: wrong second-step codes, by network and account; steps:
	// sign-ins waiting for theirs.
	codeAttempts *limit.Tries
	steps        pendingSteps
	stepped      stepped     // sign-ins whose second step was just shown
	signIns      signIns     // people's IRC passwords, for a store that keeps none
	signInLocks  limit.Locks // by network|account: one sign-in at a time
	repos        reposByInstallation
	sleep        sleeper // networks' use, and those waking (sleep.go)
	// githubConnects: connecting GitHub from networks' consoles.
	githubConnects consoleGitHubConnects
	ceremonies     ceremonies // passkeys being added
	// Signing in on the home page by email: the links out, and how many
	// went to each address and from each visitor.
	emailSignIns                   emailSignIns
	linksByAddress, linksByVisitor *limit.Tries
	// inviteMails: invites each network has emailed today, so none can use
	// GuildNet's address to write to the world.
	inviteMails *limit.Tries
	chats       openChats // chat sockets, by sign-in
	// agentLimits: outside agents' calls a minute, and open inbox checks.
	agentLimits agentLimits
	// sandboxWakes: when the sandbox VM was last asked to start.
	sandboxWakes sandboxWakes
	// storage: networks' desks' storage, as sandboxd last said (desk_storage.go).
	storage deskStorage
	// desks: desks at work now, for plans' desks at once.
	desks desksAtWork
	hooks Hooks // the extension's
	// version is this deploy's, as browsers see it (version.go).
	version  string
	creating sync.Mutex // one network created at a time
	handoffs handoffs   // one-time sign-ins to a new network's console
	firstRun firstRun   // setting a self-hosted install's network up
	seen     lastSeen   // when people were last noted as signed in
	// githubApp mints tokens for the public GitHub App's installations.
	githubApp interface {
		Token(ctx context.Context, installation int64, repos []string, write bool) (string, time.Time, error)
	}
	connecting    pendingConnects // connectors being connected, by state
	connectorHTTP *http.Client    // reaches services: public HTTPS only
	gitHTTP       *http.Client    // reaches git hosts: connectors', and private ones named
	// working: what was begun in the background (setting networks up and
	// removing them), which Finish waits for.
	working sync.WaitGroup
}

// background does work after the request that began it, counted for Finish.
func (p *Platform) background(work func()) {
	p.working.Add(1)
	go func() {
		defer p.working.Done()
		work()
	}()
}

// Finish waits until what the platform began in the background is done, or
// ctx ends: when it's told to stop (the old copy, at a deploy), a network
// it's setting up isn't left stuck.
func (p *Platform) Finish(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.working.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.Join(errors.New("stopping with work still going"), ctx.Err())
	}
}

// sessionTTL is how long a sign-in lasts; sessionIdle how long it lasts
// unused, which each use puts off (noting it at most every
// sessionTouchEvery).
const (
	sessionTTL        = 7 * 24 * time.Hour
	sessionIdle       = 24 * time.Hour
	sessionTouchEvery = 5 * time.Minute
)

func New(opts Options) *Platform {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// A self-hosted install's one network is already running: its runner
	// fetches its settings and keys from the broker.
	if opts.FirstRun != nil && opts.Host == nil {
		opts.Host, opts.KeysByBroker = fixedHost{opts.FirstRun}, true
	}
	p := &Platform{opts: opts, pages: map[string]*template.Template{}, codeAttempts: limit.New(10, 15*time.Minute),
		inviteMails: limit.New(inviteMailsADay, 24*time.Hour), linksByAddress: limit.New(linksAnAddress, time.Hour), linksByVisitor: limit.New(linksAVisitor, time.Hour)}
	p.version = buildVersion(opts.ChatDir)
	p.connectorHTTP = newConnectorHTTP(opts.ConnectorEgress)
	p.gitHTTP = egress.Client(p.gitEgress())
	if opts.GitHub != nil {
		if app, err := newGitHubApp(opts.GitHub); err != nil {
			monitor.Problem(opts.Logger, "the GitHub app isn't usable: %v", err)
		} else {
			p.githubApp = app
		}
	}
	p.hooks = p.extend()
	ext := p.hooks.Pages
	if ext == nil {
		ext = &Pages{}
	}
	// The brand's files first: they're the console's own, changed.
	ownStatic, _ := fs.Sub(staticFiles, "static")
	p.staticFiles = layers{ownStatic}
	if opts.Brand.Files != nil {
		p.staticFiles = layers{opts.Brand.Files, ownStatic}
	}
	if ext.Static != nil {
		p.staticFiles = append(p.staticFiles, ext.Static)
	}
	p.static = staticURL(opts.Brand.Files, ownStatic, ext.Static)
	funcs := template.FuncMap{"usd": usd, "join": strings.Join, "list": func(xs ...string) []string { return xs }, "initial": initial, "static": p.static,
		"mb": megabytes, "when": when, "count": count, "chart": chartOf, "measure": describeMeasure, "minus": func(a, b int) int { return a - b }, "mod": func(a, b int) int { return a % b },
		"brand": func() string { return p.brand().Name }, "tagline": func() string { return p.brand().Tagline },
		"brandLogo": func() string { return p.brandFile("logo.svg") }, "brandCSS": func() string { return p.brandFile("brand.css") },
		"source": func() string { return opts.Source }}
	for name, f := range ext.Funcs {
		if _, ok := funcs[name]; ok {
			panic("platform: the extension's template function " + name + " is the console's")
		}
		funcs[name] = f
	}
	// The extension's header and footer, if it has its own, in place of the
	// console's on every page.
	framed := func(t *template.Template) *template.Template {
		if ext.Frame != "" {
			template.Must(t.ParseFS(ext.FS, ext.Frame+".html"))
		}
		return t
	}
	for _, page := range []string{"signin", "welcome", "org", "staff", "teams", "approvals", "chat", "settings", "deleted", "connect", "not-found",
		"people", "account", "join", "reset", "link-gone", "spend", "server", "person", "team", "company", "access", "log", "roles", "agent-token", "agent-webhook", "connectors", "connect-onward", "two-step", "signin-two-step", "guest"} {
		p.pages[page] = framed(template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFiles, "templates/layout.html", "templates/parts.html", "templates/"+page+".html")))
	}
	for _, page := range []string{"home-setup", "home-signin", "home-signin-email", "home-signin-two-step", "home-two-step", "home-networks", "home-new", "home-network", "home-onward", "suspended", "waking", "home-not-found"} {
		p.pages[page] = framed(template.Must(template.New("home.html").Funcs(funcs).ParseFS(templateFiles, "templates/home.html", "templates/parts.html", "templates/"+page+".html")))
	}
	p.email = template.Must(template.New("email.html").Funcs(funcs).ParseFS(templateFiles, "templates/email.html"))
	p.sectionPages = template.New("sections").Funcs(funcs)
	if len(ext.Sections) > 0 {
		var files []string
		for _, s := range ext.Sections {
			files = append(files, s+".html")
		}
		template.Must(p.sectionPages.ParseFS(ext.FS, files...))
	}
	// The extension's pages, with the console's shared parts.
	withPartials := func(name string) []string {
		var files []string
		for _, partial := range ext.Partials {
			files = append(files, partial+".html")
		}
		return append(files, name+".html")
	}
	for _, page := range ext.Site {
		t := framed(template.Must(template.New(page+".html").Funcs(funcs).ParseFS(templateFiles, "templates/parts.html")))
		p.addPage(page, template.Must(t.ParseFS(ext.FS, withPartials(page)...)))
	}
	for _, page := range ext.Home {
		t := framed(template.Must(template.New("home.html").Funcs(funcs).ParseFS(templateFiles, "templates/home.html", "templates/parts.html")))
		p.addPage(page, template.Must(t.ParseFS(ext.FS, withPartials(page)...)))
	}
	if opts.FirstRun != nil {
		p.startFirstRun()
	}
	return p
}

// addPage adds one of the extension's pages.
func (p *Platform) addPage(name string, t *template.Template) {
	if _, ok := p.pages[name]; ok {
		panic("platform: the extension's page " + name + " is the console's")
	}
	p.pages[name] = t
}

// layers are static files, the console's own and then the extension's: a
// file is the first layer's that has it.
type layers []fs.FS

func (l layers) Open(name string) (fs.File, error) {
	for _, fsys := range l {
		if f, err := fsys.Open(name); err == nil {
			return f, nil
		}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// staticURL gives a static file's address with a version taken from its
// contents. Static files are cached as immutable, so a release that changes
// one must also change its address. The brand's files (nil if none) take
// the place of the console's own; the extension's (nil if none) having a
// file of the same name as the console's is a mistake.
func staticURL(brand, own, ext fs.FS) func(name string) (string, error) {
	urls := map[string]string{}
	for layer, fsys := range []fs.FS{brand, own, ext} {
		if fsys == nil {
			continue
		}
		fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			if _, ok := urls[path]; ok && layer == 1 {
				return nil // the brand's, in its place
			}
			if _, ok := urls[path]; ok {
				panic("platform: the extension's static file " + path + " is the console's")
			}
			sum := sha256.Sum256(b)
			urls[path] = "/static/" + path + "?v=" + hex.EncodeToString(sum[:6])
			return nil
		})
	}
	return func(name string) (string, error) {
		if u, ok := urls[name]; ok {
			return u, nil
		}
		return "", errors.New("no static file " + name)
	}
}

func (p *Platform) cookieName() string {
	if p.opts.SecureCookies {
		return "__Host-guildnet"
	}
	return "guildnet"
}

type ctxKey int

const (
	networkKey ctxKey = iota
	sessionKey
)

func networkOf(r *http.Request) Network {
	n, _ := r.Context().Value(networkKey).(Network)
	return n
}

func sessionOf(r *http.Request) (Session, bool) {
	s, ok := r.Context().Value(sessionKey).(sessionWithToken)
	return s.Session, ok
}

type sessionWithToken struct {
	Session
	hash string
}

// Handler serves the platform: static files anywhere, the home page on the
// base domain, and each network's console on its own address.
func (p *Platform) Handler() http.Handler {
	static := p.staticFiles
	files := http.FileServerFS(static)

	home := p.homeHandler()
	console := http.NewServeMux()
	console.HandleFunc("GET /signin", p.signinPage)
	console.HandleFunc("GET /welcome", p.welcome)
	console.HandleFunc("POST /welcome", p.openWelcome)
	console.HandleFunc("POST /signin", p.signin)
	console.HandleFunc("GET /signin/two-step", p.secondStepPage)
	console.HandleFunc("POST /signin/two-step", p.secondStepDone)
	console.HandleFunc("POST /signin/two-step/passkey/begin", p.beginPasskeyStep)
	console.HandleFunc("POST /signin/two-step/passkey/finish", p.finishPasskeyStep)
	console.HandleFunc("POST /signout", p.signedIn(p.signout))
	console.HandleFunc("GET /{$}", p.signedIn(p.orgPage))
	console.HandleFunc("POST /staff", p.signedIn(p.hire))
	console.HandleFunc("GET /staff/{nick}", p.signedIn(p.staffPage))
	console.HandleFunc("POST /staff/{nick}", p.signedIn(p.saveAgent))
	console.HandleFunc("POST /staff/{nick}/fire", p.signedIn(p.fire))
	console.HandleFunc("POST /staff/{nick}/token", p.signedIn(p.newAgentToken))
	console.HandleFunc("POST /staff/{nick}/webhook", p.signedIn(p.setAgentWebhook))
	console.HandleFunc("POST /staff/{nick}/desk/clear", p.signedIn(p.clearDesk))
	console.HandleFunc("GET /teams", p.signedIn(p.teamsPageHandler))
	console.HandleFunc("POST /teams", p.signedIn(p.makeTeam))
	console.HandleFunc("GET /teams/{name}", p.signedIn(p.teamPageHandler))
	console.HandleFunc("POST /teams/{name}", p.signedIn(p.saveTeam))
	console.HandleFunc("GET /company", p.signedIn(p.companyPageHandler))
	console.HandleFunc("POST /company", p.signedIn(p.saveCompany))
	console.HandleFunc("POST /goals", p.signedIn(p.addGoal))
	console.HandleFunc("POST /goals/{id}", p.signedIn(p.saveGoal))
	console.HandleFunc("GET /access", p.signedIn(p.accessPageHandler))
	console.HandleFunc("POST /access", p.signedIn(p.saveAccess))
	console.HandleFunc("POST /access/{name}", p.signedIn(p.saveAccess))
	console.HandleFunc("GET /log", p.signedIn(p.logPage))
	console.HandleFunc("GET /roles", p.signedIn(p.rolesPage))
	// Teams used to be called lanes.
	console.HandleFunc("GET /lanes", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/teams", http.StatusMovedPermanently)
	})
	console.HandleFunc("GET /approvals", p.signedIn(p.approvalsPage))
	console.HandleFunc("POST /approvals/after", p.signedIn(p.setMoveAfter))
	console.HandleFunc("POST /approvals/{id}/{decision}", p.signedIn(p.decide))
	console.HandleFunc("GET /chat/{$}", p.signedIn(p.chatPage))
	console.HandleFunc("GET /chat/app/", p.chatApp)
	console.HandleFunc("POST /chat/token", p.chatToken)
	console.HandleFunc("POST /chat/guildserv", p.chatGuildServ)
	console.HandleFunc("GET /chat/socket", p.chatSocket)
	console.HandleFunc("GET /connect", p.signedIn(p.connectPage))
	console.HandleFunc("GET /connectors", p.signedIn(p.connectorsPage))
	console.HandleFunc("POST /connectors/{connector}/tools/{tool}", p.signedIn(p.setConnectorTool))
	// The owner connects GitHub and services here; each comes back by way of
	// the home page.
	console.HandleFunc("POST /connectors/{connector}/connect", p.ownerOnly(p.connectHere))
	console.HandleFunc("POST /connectors/custom", p.ownerOnly(p.addCustomConnector))
	console.HandleFunc("POST /connectors/{connector}/disconnect", p.ownerOnly(p.disconnectHere))
	console.HandleFunc("GET /oauth/callback", p.ownerOnly(p.connectedHere))
	console.HandleFunc("POST /github/connect", p.ownerOnly(p.connectGitHubHere))
	console.HandleFunc("GET /github/callback", p.ownerOnly(p.connectedGitHubHere))
	console.HandleFunc("POST /github/disconnect", p.ownerOnly(p.disconnectGitHubHere))
	console.HandleFunc("POST /git", p.ownerOnly(p.addGitRepo))
	console.HandleFunc("POST /git/{name}/test", p.ownerOnly(p.testGitRepo))
	console.HandleFunc("POST /git/{name}/remove", p.ownerOnly(p.removeGitRepo))
	if p.deskHostsOn() {
		// The runner decides who may: the owner adds and removes, others ask.
		console.HandleFunc("POST /desk-hosts", p.signedIn(p.addDeskHost))
		console.HandleFunc("POST /desk-hosts/ask", p.signedIn(p.askForDeskHost))
		console.HandleFunc("POST /desk-hosts/{host}/remove", p.signedIn(p.removeDeskHost))
	}
	console.HandleFunc("GET /server", p.signedIn(p.serverPage))
	console.HandleFunc("POST /server/test-email", p.signedIn(p.sendTestEmail))
	console.HandleFunc("GET /settings", p.signedIn(p.settingsPage))
	console.HandleFunc("POST /settings", p.signedIn(p.saveTimezone))
	console.HandleFunc("POST /settings/keys", p.signedIn(p.saveKey))
	console.HandleFunc("POST /settings/keys/remove", p.signedIn(p.removeKey))
	console.HandleFunc("POST /settings/delete", p.signedIn(p.deleteNetwork))
	if p.hooks.Console != nil {
		p.hooks.Console(routes{p: p, mux: console})
	}
	console.HandleFunc("POST /onboarding/{step}", p.signedIn(p.tickOff))
	console.HandleFunc("GET /spend", p.signedIn(p.spendPage))
	console.HandleFunc("GET /people", p.signedIn(p.peoplePage))
	// Where a boosted form leaves the address bar; reloading goes back to the
	// page (a link, shown once, isn't shown again).
	console.HandleFunc("GET /people/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/people", http.StatusSeeOther) })
	console.HandleFunc("POST /people/invite", p.signedIn(p.invitePerson))
	console.HandleFunc("POST /people/invites/{id}/cancel", p.signedIn(p.cancelInvite))
	console.HandleFunc("POST /people/guests", p.signedIn(p.setGuestsAsk))
	console.HandleFunc("POST /people/{account}/reset", p.signedIn(p.makeResetLink))
	console.HandleFunc("GET /people/{account}", p.signedIn(p.personPageHandler))
	console.HandleFunc("POST /people/{account}", p.signedIn(p.savePerson))
	console.HandleFunc("POST /people/{account}/access", p.signedIn(p.personAccess))
	console.HandleFunc("POST /people/{account}/lead", p.signedIn(p.personLead))
	console.HandleFunc("POST /people/{account}/remove", p.signedIn(p.removePerson))
	console.HandleFunc("GET /account", p.signedIn(p.accountPage))
	console.HandleFunc("POST /account/password", p.signedIn(p.changeOwnPassword))
	console.HandleFunc("POST /account/app-passwords", p.signedIn(p.makeAppPassword))
	console.HandleFunc("POST /account/app-passwords/{id}/revoke", p.signedIn(p.revokeAppPassword))
	if p.opts.TwoSteps != nil {
		console.HandleFunc("GET /account/two-step", p.signedIn(p.twoStepPage))
		console.HandleFunc("POST /account/two-step", p.signedIn(p.setUpTwoStep))
		console.HandleFunc("POST /account/two-step/off", p.signedIn(p.turnOffTwoStep))
		console.HandleFunc("GET /account/two-step/codes", p.signedIn(p.freshCodes))
		console.HandleFunc("POST /account/passkeys/begin", p.signedIn(p.beginAddPasskey))
		console.HandleFunc("POST /account/passkeys/finish", p.signedIn(p.finishAddPasskey))
		console.HandleFunc("POST /account/passkeys/{id}/remove", p.signedIn(p.removePasskey))
		console.HandleFunc("POST /people/{account}/two-step/reset", p.signedIn(p.resetSomeonesTwoStep))
	}
	console.HandleFunc("POST /account/alerts/email", p.signedIn(p.giveAlertAddress))
	console.HandleFunc("POST /account/alerts", p.signedIn(p.setAlerts))
	console.HandleFunc("GET /account/email/confirm/{token}", p.confirmAlertAddress)
	console.HandleFunc("GET /join/{token}", p.joinPage)
	console.HandleFunc("POST /join/{token}", p.join)
	console.HandleFunc("GET /reset/{token}", p.resetPage)
	console.HandleFunc("POST /reset/{token}", p.reset)
	console.HandleFunc("GET /robots.txt", consoleRobots)
	console.HandleFunc("/", p.notFound)

	return p.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			http.StripPrefix("/static/", files).ServeHTTP(w, r)
			return
		}
		// Where browsers and phones look for an icon without being told, on
		// every site: fixed addresses, so cached for a day only.
		if kind, ok := rootIcons[r.URL.Path]; ok && (r.Method == "GET" || r.Method == "HEAD") {
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			http.ServeFileFS(w, r, static, strings.TrimPrefix(r.URL.Path, "/"))
			return
		}
		// Agents' programs call with a token, not cookies: nothing a page on
		// another site could borrow.
		agentCall := strings.HasPrefix(r.URL.Path, "/agents/") || strings.HasPrefix(r.URL.Path, "/models/")
		if r.Method != "GET" && r.Method != "HEAD" && !sameOrigin(r) && !agentCall {
			http.Error(w, "That came from another site, so it was refused.", http.StatusForbidden)
			return
		}
		// Forms are small; agents' calls have their own limits.
		if !agentCall && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		}
		host := hostOnly(r.Host)
		if host == p.opts.BaseDomain {
			// Only the public pages are for search engines (the extension's
			// say). What the admins do says it was done (adminPage); the rest
			// of the home page says what happened in its own words.
			if p.hooks.Indexable == nil || !p.hooks.Indexable(r.URL.Path) {
				w.Header().Set("X-Robots-Tag", noindex)
			}
			home.ServeHTTP(w, r)
			return
		}
		name, ok := strings.CutSuffix(host, "."+p.opts.BaseDomain)
		// www is the home page, at one address.
		if ok && name == "www" {
			http.Redirect(w, r, p.origin()+r.URL.RequestURI(), http.StatusMovedPermanently)
			return
		}
		if !ok || name == "" || strings.Contains(name, ".") {
			http.NotFound(w, r)
			return
		}
		n, err := p.opts.Networks.Network(r.Context(), name)
		if errors.Is(err, ErrNotFound) || (err == nil && n.Status == Deleting) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			monitor.Problem(p.opts.Logger, "finding network %s: %v", name, err)
			http.Error(w, "Something went wrong. Try again in a moment.", http.StatusInternalServerError)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/models/") {
			if p.hooks.Models == nil {
				gatewayError(w, http.StatusNotFound, "there's no model gateway here: call your model provider with keys of your own", "invalid_request_error")
				return
			}
			p.hooks.Models.Serve(w, r, n)
			return
		}
		if agentCall {
			p.agentAPI(w, r, n)
			return
		}
		if n.Status == Suspended {
			p.renderHome(w, r, http.StatusServiceUnavailable, "suspended", withError{View: struct{ Name, Contact string }{n.Name, p.address("support")}})
			return
		}
		// A network's console is never for search engines.
		w.Header().Set("X-Robots-Tag", noindex)
		if n.Status == Asleep && p.opts.Host != nil {
			p.waking(w, r, n)
			return
		}
		ctx := context.WithValue(r.Context(), networkKey, n)
		if s, ok := p.loadSession(r, n); ok {
			ctx = context.WithValue(ctx, sessionKey, s)
			ctx = context.WithValue(ctx, meKey, &meMemo{})
			p.noteUsed(r.Context(), n)
		}
		console.ServeHTTP(p.markDone(w, r), r.WithContext(ctx))
	}))
}

// maxForm is the most a page's form (or Stripe's webhook) may send.
const maxForm = 1 << 20

// visitor is the address a request came from. Behind a trusted edge that's
// the last entry in X-Forwarded-For, the one the edge added itself; anything
// before it was sent by the visitor and could be made up.
func (p *Platform) visitor(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	for _, trusted := range p.opts.TrustedProxies {
		if trusted.Contains(peer.Unmap()) {
			chain := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
			if last, err := netip.ParseAddr(strings.TrimSpace(chain[len(chain)-1])); err == nil {
				return last.Unmap().String()
			}
			break
		}
	}
	return peer.Unmap().String()
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(hostport)
}

// sameOrigin says whether a change came from this site's own pages, going by
// what browsers send: Origin, or failing that Sec-Fetch-Site.
func sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == "https://"+r.Host || origin == "http://"+r.Host
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// securityHeaders sets a strict policy on everything: scripts and styles
// only from this site, no framing, no inline code.
func (p *Platform) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp(""))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if p.opts.SecureCookies {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// csp is the site's Content Security Policy, with extra directives for the
// odd page that needs them (the chat page frames the chat app; the chat app
// may be framed by this site). Everything else frames nothing and can't be
// framed.
func csp(extra string) string {
	policy := map[string]string{
		"default-src": "'none'", "script-src": "'self'", "style-src": "'self'", "font-src": "'self'",
		"img-src": "'self' data:", "connect-src": "'self'", "manifest-src": "'self'", "form-action": "'self'",
		"frame-ancestors": "'none'", "base-uri": "'none'",
	}
	order := []string{"default-src", "script-src", "style-src", "font-src", "img-src", "connect-src", "manifest-src", "form-action", "frame-ancestors", "base-uri"}
	for _, d := range strings.Split(extra, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		if name == "" {
			continue
		}
		if _, known := policy[name]; !known {
			order = append(order, name)
		}
		policy[name] = value
	}
	var out []string
	for _, name := range order {
		out = append(out, name+" "+policy[name])
	}
	return strings.Join(out, "; ")
}

// loadSession finds the session a request's cookie belongs to, if it's for
// this network and hasn't expired.
func (p *Platform) loadSession(r *http.Request, n Network) (sessionWithToken, bool) {
	c, err := r.Cookie(p.cookieName())
	if err != nil || c.Value == "" {
		return sessionWithToken{}, false
	}
	hash := hashToken(c.Value)
	s, err := p.opts.Sessions.Get(r.Context(), hash)
	now := p.opts.Now()
	if err != nil || !strings.EqualFold(s.Network, n.Name) || s.Ended(now) {
		return sessionWithToken{}, false
	}
	// Used: it lasts another day unused, up to its week.
	if !s.IdleUntil.IsZero() && s.IdleUntil.Sub(now) < sessionIdle-sessionTouchEvery {
		s.IdleUntil = minTime(now.Add(sessionIdle), s.Expires)
		if err := p.opts.Sessions.Touch(r.Context(), hash, s.IdleUntil); err != nil {
			monitor.Problem(p.opts.Logger, "noting a sign-in on %s was used: %v", n.Name, err)
		}
	}
	return sessionWithToken{Session: s, hash: hash}, true
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// closeEndedChats closes the open chats of sign-ins that have ended
// (unused a day, or a week on) since they opened.
func (p *Platform) closeEndedChats(ctx context.Context) {
	now := p.opts.Now()
	for _, hash := range p.chats.sessions() {
		s, err := p.opts.Sessions.Get(ctx, hash)
		if errors.Is(err, ErrNotFound) || (err == nil && s.Ended(now)) {
			p.chats.closeAll(hash)
		}
	}
}

// signedIn serves h only to someone signed in to this network; changes must
// also carry the session's CSRF token.
func (p *Platform) signedIn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := sessionOf(r)
		if !ok {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			token := r.PostFormValue("csrf")
			if token == "" {
				token = r.Header.Get("X-CSRF-Token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRF)) != 1 {
				http.Error(w, "That form has expired. Go back, reload the page, and try again.", http.StatusForbidden)
				return
			}
		}
		if !p.mayGoBeforeTwoStep(r) {
			first, err := p.setUpFirst(r)
			if err != nil {
				monitor.Problem(p.opts.Logger, "checking %s's two-step sign-in on %s: %v", s.Account, s.Network, err)
				http.Error(w, "The network didn't answer. Try again in a moment.", http.StatusBadGateway)
				return
			}
			if first {
				if r.Method == "GET" || r.Method == "HEAD" {
					http.Redirect(w, r, "/account/two-step", http.StatusSeeOther)
					return
				}
				http.Error(w, "Set up two-step sign-in first, on your account page.", http.StatusForbidden)
				return
			}
		}
		h(w, r)
	}
}

// mayGoBeforeTwoStep says whether a request is one someone who must set up
// two-step sign-in may make before they have: setting it up, and signing
// out.
func (p *Platform) mayGoBeforeTwoStep(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/account/two-step") || strings.HasPrefix(r.URL.Path, "/account/passkeys/") ||
		(r.Method == "POST" && r.URL.Path == "/signout")
}

// startSession signs account in to network n.
func (p *Platform) startSession(w http.ResponseWriter, r *http.Request, n Network, account string) error {
	_, err := p.newSession(w, r, n, account)
	return err
}

// newSession signs account in to n, and is the sign-in's hash.
func (p *Platform) newSession(w http.ResponseWriter, r *http.Request, n Network, account string) (string, error) {
	token := newToken()
	now := p.opts.Now()
	s := Session{Network: n.Name, Account: account, CSRF: newToken(), Expires: now.Add(sessionTTL), IdleUntil: now.Add(sessionIdle)}
	if err := p.opts.Sessions.Save(r.Context(), hashToken(token), s); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	return hashToken(token), nil
}

func (p *Platform) endSession(w http.ResponseWriter, r *http.Request) {
	if s, ok := r.Context().Value(sessionKey).(sessionWithToken); ok {
		p.opts.Sessions.Delete(r.Context(), s.hash)
		p.chats.closeAll(s.hash)
	}
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
}

// signOutEverywhere ends every console sign-in of account's on n, and
// closes their chats.
func (p *Platform) signOutEverywhere(ctx context.Context, n Network, account string) {
	if err := p.opts.Sessions.DeleteAccount(ctx, n.Name, account, ""); err != nil {
		monitor.Problem(p.opts.Logger, "signing %s out of %s: %v", account, n.Name, err)
	}
	p.chats.closeAll(chatsOf(n.Name, account))
}

// signOutElsewhere ends account's console sign-ins on n but the session
// keep, and closes their chats: chats open anywhere were let in on sign-ins
// that no longer count. The kept session's chat opens again.
func (p *Platform) signOutElsewhere(ctx context.Context, n Network, account, keep string) {
	if err := p.opts.Sessions.DeleteAccount(ctx, n.Name, account, keep); err != nil {
		monitor.Problem(p.opts.Logger, "signing %s out of %s: %v", account, n.Name, err)
	}
	p.chats.closeAll(chatsOf(n.Name, account))
}
