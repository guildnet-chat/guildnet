// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/monitor"
)

// The console is open source; GuildNet's hosted edition adds what isn't
// (plans, desk hours, billing...) through an Extension. Without one, the
// console runs on its own: no plans, and desk time isn't counted.

// Extension adds to the console. Extend is called once, from New.
type Extension interface {
	Extend(c *Core) Hooks
}

// ExtensionFunc is a function that's an Extension.
type ExtensionFunc func(c *Core) Hooks

func (f ExtensionFunc) Extend(c *Core) Hooks { return f(c) }

// Hooks are what an extension adds. A nil hook adds nothing.
type Hooks struct {
	// Desks decides whether a network's desks may work and counts their
	// time; nil means desk time isn't counted (runners take the broker's
	// 404 to mean as many desks as the sandbox has room for).
	Desks DeskPolicy
	// Settings are sections of a network's Settings, for its owner, after
	// the company's timezone, in this order.
	Settings []SettingsSection
	// Console adds pages to every network's console.
	Console func(Routes)
	// FormAction are other places the console's forms may send people (a
	// payment provider's pages), on signed-in pages.
	FormAction []string
	// BeforeDelete has a say before a network's owner deletes it: a refusal
	// says why not yet.
	BeforeDelete func(ctx context.Context, n Network) (refusal string, err error)

	// Home adds pages to the home page, on the base domain. Its own "GET
	// /{$}" or "GET /robots.txt" take the place of the open home page's.
	Home func(Routes)
	// IsAdmin says who may see the extension's admin pages (Admins); nil
	// means nobody.
	IsAdmin func(Person) bool
	// Indexable says which pages on the base domain search engines may
	// index; nil means none.
	Indexable func(path string) bool

	// MayCreate says whether person, who owns owned networks here, may
	// create another; nil means the open edition's rule: one network here,
	// made by whoever sets it up.
	MayCreate func(ctx context.Context, person Person, owned int) (CreateAnswer, error)
	// Checklist are steps added to the end of each network's
	// getting-started checklist.
	Checklist []ChecklistStep
	// Jobs are what the extension does now and then, beside the console's
	// own (Platform.Jobs).
	Jobs []Job

	// Models is a model gateway: agents that run outside GuildNet calling
	// models through it. nil means none; they use keys of their own.
	Models *ModelGateway

	// Pages are the extension's own templates and static files, which Core's
	// Render and RenderHome draw.
	Pages *Pages
	// AgentDocs is where the docs for agents that run outside are (an
	// address on the home page's site), which their token page points to;
	// "" means there are none to point to.
	AgentDocs string
}

// Pages are an extension's templates, as html/template files, and its
// static files. Each page has the console's shared parts (parts.html: the
// site's header and footer, its icons...), the console's template
// functions, Funcs, and the Partials. A name the console has already, for
// a page, a function or a static file, is a mistake, and stops it starting.
type Pages struct {
	// FS has a file <name>.html for each page and partial named below.
	FS fs.FS
	// Site are pages of the public site, each a whole page drawn with Render:
	// their data is Core.Render's, as .Data, and the site's header and footer
	// take .Site.
	Site []string
	// Home are pages in the home page's frame (home.html), drawn with
	// RenderHome: each defines "main" (and may define "title", "head" and
	// "wide"); their data is RenderHome's, as .Data.
	Home []string
	// Partials are templates every page has (defined with {{define}}).
	Partials []string
	// Frame, if set, is a template defining "site-header" and "site-footer":
	// the site's header and footer, in place of the console's own, on every
	// page with them (the console's own pages too). They're given a site
	// (see parts.html).
	Frame string
	// Sections are templates of parts of pages, each defining one by its
	// name, drawn with Core.RenderSection: for SettingsSection.Render.
	Sections []string
	Funcs    template.FuncMap
	// Static are served at /static/, beside the console's own, at the
	// addresses the "static" function gives.
	Static fs.FS
}

// ModelGateway answers outside agents' model calls, at /models/ on their
// network's address. Those calls carry the agent's token, not a session.
type ModelGateway struct {
	Serve func(w http.ResponseWriter, r *http.Request, n Network)
	// On says whether it's on for n, as its agents' pages say.
	On func(r *http.Request, n Network) bool
}

// CreateAnswer is whether someone may create a network: Why, if set, is why
// not, which they're told if they try; Note is shown on their networks page
// instead of the button to create one.
type CreateAnswer struct {
	Why  string
	Note template.HTML
}

// ChecklistStep is a step of the getting-started checklist: Done, if set,
// says whether it's done without the owner ticking it off.
type ChecklistStep struct {
	Key, Title, Hint, Link, LinkText string
	Done                             func(r *http.Request, n Network) bool
}

// Job is something to do every so often.
type Job struct {
	Every time.Duration
	Run   func(ctx context.Context)
}

// SettingsSection is a section of a network's Settings: Render is its HTML
// for n's owner, or "" for none. A section that fails is left out.
type SettingsSection struct {
	Render func(r *http.Request, n Network) (template.HTML, error)
}

// Access is who may reach an extension's page.
type Access int

const (
	// Anyone may reach it.
	Anyone Access = iota
	// SignedIn is someone signed in, as the console's own pages are (in a
	// console, the network's people; on the home page, someone signed in
	// there): anyone else is sent to sign in, and forms must carry the
	// session's CSRF token.
	SignedIn
	// Admins is, on the home page, someone signed in there whom IsAdmin
	// says is an admin; to anyone else the page isn't there at all. Forms
	// must carry the CSRF token, and a form that went through says so on
	// the next page.
	Admins
)

// Routes are where an extension adds its pages, as http.ServeMux patterns.
// A pattern the console has already is a mistake, and stops it starting.
type Routes interface {
	Handle(pattern string, a Access, h http.HandlerFunc)
}

type routes struct {
	p       *Platform
	mux     *http.ServeMux
	home    bool            // the home page's, not a console's
	claimed map[string]bool // patterns added, for the home page's own to make way
}

func (rt routes) Handle(pattern string, a Access, h http.HandlerFunc) {
	switch {
	case a == SignedIn && rt.home:
		h = rt.p.person(h)
	case a == SignedIn:
		h = rt.p.signedIn(h)
	case a == Admins && rt.home:
		h = rt.p.adminPage(h)
	case a == Admins:
		panic("platform: admin pages are on the home page: " + pattern)
	}
	if rt.claimed != nil {
		rt.claimed[pattern] = true
	}
	rt.mux.HandleFunc(pattern, h)
}

// DeskPolicy decides whether a network's desk may start, and counts the time
// desks worked. The broker keeps the desks at work and checks the runner;
// the policy only decides and counts.
type DeskPolicy interface {
	// Limits is where the network stands before one of agent's desks
	// starts.
	Limits(ctx context.Context, n Network, agent string) (DeskLimits, error)
	// Worked counts seconds of agent's desk time, at most the time that
	// passed since its desk started.
	Worked(ctx context.Context, n Network, agent string, seconds int64, at time.Time) error
}

// DeskLimits are what a desk policy says before a desk starts.
type DeskLimits struct {
	// Refused, if set, is why the desk may not start: a new piece of work
	// is turned away (402), but work under way, after an approval, isn't.
	Refused string
	// AtOnce is how many of the network's desks may work at once; 0 means
	// no limit but the sandbox's.
	AtOnce int
	// Plan names what the network is on, for the runner's logs.
	Plan string
}

// Core is what an extension may use of the console.
type Core struct{ p *Platform }

func (p *Platform) core() *Core { return &Core{p: p} }

// Now is the console's clock.
func (c *Core) Now() time.Time { return c.p.opts.Now() }

// NetworkOf is the network a console request is for.
func (c *Core) NetworkOf(r *http.Request) Network { return networkOf(r) }

// CSRF is the token a signed-in person's forms carry (in a console, or on
// the home page), or "".
func (c *Core) CSRF(r *http.Request) string {
	if s, ok := sessionOf(r); ok {
		return s.CSRF
	}
	return homeOf(r).CSRF
}

// Logger is where the console logs.
func (c *Core) Logger() *log.Logger { return c.p.opts.Logger }

// Brand is what this GuildNet goes by, its addresses filled in.
func (c *Core) Brand() Brand { return c.p.brand() }

// Origin is the home page's address, like https://guildnet.chat.
func (c *Core) Origin() string { return c.p.origin() }

// Static is the address of one of the static files, the console's or the
// extension's, with its version.
func (c *Core) Static(name string) (string, error) { return c.p.static(name) }

// Visitor is the address a request came from: behind the trusted edge, the
// one the edge saw.
func (c *Core) Visitor(r *http.Request) string { return c.p.visitor(r) }

// HomePerson is who is signed in on the home page, if anyone.
func (c *Core) HomePerson(r *http.Request) (Person, bool) {
	if s := homeOf(r); s.person.ID != "" {
		return s.person, true
	}
	s, ok := c.p.loadHomeSession(r)
	return s.person, ok
}

// CanSignIn says whether there's a way to sign in on the home page.
func (c *Core) CanSignIn() bool { return c.p.canSignIn() }

// Render draws one of the extension's Site pages with data (as .Data),
// with status. Data with a method ErrorText() string shows that problem on
// the page; a form that just went through says so.
func (c *Core) Render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	c.p.render(w, r, status, page, data)
}

// RenderHome draws one of the extension's Home pages with data, as Render
// does, in the home page's frame.
func (c *Core) RenderHome(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	c.p.renderHome(w, r, status, page, data)
}

// Email is l as an email in the console's style: plain text, and the same
// in HTML.
func (c *Core) Email(l Letter) mail.Message { return c.p.message(l) }

// Mail sends the console's email; nil means none is sent.
func (c *Core) Mail() mail.Sender { return c.p.opts.Mail }

// Networks are the console's networks.
func (c *Core) Networks() Networks { return c.p.opts.Networks }

// People are who sign in on the home page; nil means nobody does.
func (c *Core) People() People { return c.p.opts.People }

// CallRunner calls n's runner's API, as the platform: in (if not nil) goes
// as JSON, and out (if not nil) is filled from the JSON answer.
func (c *Core) CallRunner(ctx context.Context, n Network, method, path string, in, out any) error {
	return (runner{n: n}).call(ctx, method, path, in, out)
}

// NetworkOrigin is where n's console is, as the request came in: its name
// on the base domain, with the request's port.
func (c *Core) NetworkOrigin(r *http.Request, name string) string { return c.p.networkOrigin(r, name) }

// HomeURL is an address on the home page, as the request came in.
func (c *Core) HomeURL(r *http.Request, path string) string { return c.p.homeURL(r, path) }

// RemoveNetwork deletes n: from now on it serves nothing, and the host
// takes it away in the background.
func (c *Core) RemoveNetwork(ctx context.Context, n Network) error { return c.p.startRemoving(ctx, n) }

// NetworkURL is where network's console is, on the base domain.
func (c *Core) NetworkURL(network string) string {
	return c.p.scheme() + "://" + network + "." + c.p.opts.BaseDomain
}

// TellOwner emails l to n's owner, at the address they signed in with, if
// the console sends email. It's only ever news: a failure is logged.
func (c *Core) TellOwner(ctx context.Context, n Network, l Letter) { c.p.tellOwner(ctx, n, l) }

// OwnedHere is a console request's network, if the one signed in owns it
// (and it was created on the home page); anyone else is told no.
func (c *Core) OwnedHere(w http.ResponseWriter, r *http.Request) (Network, bool) {
	return c.p.ownedHere(w, r)
}

// RenderSettings draws n's Settings, for its owner, with problem.
func (c *Core) RenderSettings(w http.ResponseWriter, r *http.Request, status int, n Network, problem string) {
	c.p.renderSettings(w, r, status, n, problem)
}

// RenderSection draws one of the extension's Sections with data, for a
// Settings section.
func (c *Core) RenderSection(name string, data any) (template.HTML, error) {
	var b strings.Builder
	if err := c.p.sectionPages.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return template.HTML(b.String()), nil
}

// HasDesks says whether there are desks of the console's own (a sandbox).
func (c *Core) HasDesks() bool { return c.p.opts.Sandbox.URL != "" }

// DesksWorking is how many of network's desks are working now.
func (c *Core) DesksWorking(network string) int { return c.p.desksWorking(network) }

// extend is the extension's hooks; without one, the open console's.
func (p *Platform) extend() Hooks {
	if p.opts.Extension == nil {
		return Hooks{}
	}
	return p.opts.Extension.Extend(p.core())
}

// Jobs are what the platform does every so often: finishing off deleted
// networks' desks, and the extension's jobs.
func (p *Platform) Jobs() []Job {
	return append([]Job{{Every: 5 * time.Minute, Run: p.SweepDesks}, {Every: 24 * time.Hour, Run: p.checkSandboxdCertificate}}, p.hooks.Jobs...)
}

// agentDocs is where the docs for outside agents are, in full, or "".
func (p *Platform) agentDocs() string {
	if p.hooks.AgentDocs == "" {
		return ""
	}
	return p.origin() + p.hooks.AgentDocs
}

// admits says whether person is one of the extension's admins.
func (p *Platform) admits(person Person) bool {
	return p.hooks.IsAdmin != nil && p.hooks.IsAdmin(person)
}

// mayDelete is whether n's owner may delete it now, or why not.
func (p *Platform) mayDelete(ctx context.Context, n Network) (refusal string, err error) {
	if p.hooks.BeforeDelete == nil {
		return "", nil
	}
	return p.hooks.BeforeDelete(ctx, n)
}

// sections are the extension's sections of n's Settings.
func (p *Platform) sections(r *http.Request, n Network) []template.HTML {
	var out []template.HTML
	for _, s := range p.hooks.Settings {
		html, err := s.Render(r, n)
		if err != nil {
			monitor.Problem(p.opts.Logger, "a section of %s's settings: %v", n.Name, err)
			continue
		}
		if html != "" {
			out = append(out, html)
		}
	}
	return out
}
