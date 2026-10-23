// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/monitor"
)

// The console's Connectors page: GitHub and the services connected to the
// network, each tool's policy, and tools a service changed (or added)
// waiting for someone to approve them. Anyone signed in sees it; whoever
// may approve anything in the org sets it. The network's owner connects
// GitHub and services here (or, for a network made on the home page, there
// too).

// connectedService is a connected service, as the runner shows it.
type connectedService struct {
	ID        string      `json:"id"`
	Title     string      `json:"title"`
	Reconnect bool        `json:"reconnect"`
	Tools     []toolState `json:"tools"`
}

type toolState struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Policy      string `json:"policy"`
	Default     string `json:"default"`
	State       string `json:"state"`
}

// Waiting says the tool is held back until someone approves it.
func (t toolState) Waiting() bool { return t.State != "approved" }

func (p *Platform) services(r *http.Request) ([]connectedService, error) {
	var out struct {
		Connectors []connectedService `json:"connectors"`
	}
	err := p.runner(r).call(r.Context(), "GET", "/v1/connectors", nil, &out)
	return out.Connectors, err
}

type connectorsPage struct {
	Services []connectedService
	CanSet   bool // may set policies and approve
	Owner    bool // may connect and disconnect
	// What's connected, as the platform keeps it: GitHub (when it's set up
	// here) and each service on offer.
	GitHubOn   bool
	GitHub     GitHubLink
	Connectors []connectorView
	// Git repositories on any host (git_repos.go), when they can be added
	// here; the form, shown again when it wasn't taken; and how a test went.
	GitOn   bool
	Repos   []gitRepoView
	GitForm gitForm
	Tested  *gitCheck
	// Hosts desks may reach past the usual ones (desk_hosts.go), where there
	// are desks of the console's own.
	Hosts *deskHostsView
}

func (p *Platform) connectorsPage(w http.ResponseWriter, r *http.Request) {
	p.showConnectors(w, r, http.StatusOK, "")
}

func (p *Platform) showConnectors(w http.ResponseWriter, r *http.Request, status int, problem string) {
	p.showConnectorsWith(w, r, status, problem, nil)
}

// showConnectorsWith is the Connectors page, with more set by also (if any).
func (p *Platform) showConnectorsWith(w http.ResponseWriter, r *http.Request, status int, problem string, also func(*connectorsPage)) {
	m, _ := p.loadMe(r)
	n := networkOf(r)
	v := connectorsPage{CanSet: m.inTeam("approve", ""), Owner: m.Owner, GitHubOn: p.githubApp != nil, Connectors: p.connectorViews(r.Context(), n.Name),
		GitOn: p.opts.GitRepos != nil, Repos: p.gitRepoViews(r.Context(), n.Name)}
	if p.deskHostsOn() {
		hosts, err := p.deskHosts(r)
		if err != nil && problem == "" {
			status, problem = statusOf(err), messageOf(err)
		}
		v.Hosts = hosts
	}
	if also != nil {
		also(&v)
	}
	v.GitHub, _ = p.opts.Networks.GitHubOf(r.Context(), n.Name)
	if v.GitHubOn && v.Owner {
		// "Connect GitHub" goes on to GitHub.
		w.Header().Set("Content-Security-Policy", csp("form-action 'self' "+p.opts.GitHub.web()))
	}
	services, err := p.services(r)
	if err != nil && problem == "" {
		status, problem = statusOf(err), messageOf(err)
	}
	v.Services = services
	p.render(w, r, status, "connectors", withError{Err: problem, View: v})
}

// ownerOnly is a console page for the network's owner alone; it isn't there
// for anyone else.
func (p *Platform) ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return p.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if m, err := p.loadMe(r); err != nil || !m.Owner {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	})
}

// byAccount is who connected something from the console: an IRC account,
// where a home page connect has a Person's ID.
func byAccount(r *http.Request) string {
	s, _ := sessionOf(r)
	return "irc:" + s.Account
}

// inConsole says where something was connected from, for the owner's email.
func inConsole(r *http.Request) string {
	s, _ := sessionOf(r)
	return networkOf(r).Name + "'s console, signed in as " + s.Account
}

// onHome says it was the home page.
func (p *Platform) onHome(email string) string {
	return "the " + ourName + " home page, signed in as " + email
}

// connectHere starts connecting a service, from the console.
func (p *Platform) connectHere(w http.ResponseWriter, r *http.Request) {
	c, ok := connectors.Find(p.catalogue(), r.PathValue("connector"))
	if !ok || p.opts.Connections == nil {
		http.NotFound(w, r)
		return
	}
	n := networkOf(r)
	authorize, err := p.startConnect(w, r, n, c, byAccount(r), true)
	if err != nil {
		p.showConnectors(w, r, http.StatusBadGateway, "Couldn't reach "+c.Title+" to connect it. Try again in a moment.")
		return
	}
	p.render(w, r, http.StatusOK, "connect-onward", onward{Title: c.Title, Network: n.Name, URL: authorize})
}

// connectedHere is the owner coming back from a service's sign-in, passed
// on by the home page: only in the browser that started it.
func (p *Platform) connectedHere(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: p.connectCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	n := networkOf(r)
	again := "That didn't start here, or took too long. Start connecting again."
	cookie, err := r.Cookie(p.connectCookie())
	if err != nil || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(q.Get("state"))) != 1 {
		p.showConnectors(w, r, http.StatusBadRequest, again)
		return
	}
	pc, ok := p.connecting.take(q.Get("state"), p.opts.Now())
	if !ok || !pc.console || !strings.EqualFold(pc.network, n.Name) || pc.person != byAccount(r) {
		p.showConnectors(w, r, http.StatusBadRequest, again)
		return
	}
	title, status, problem := p.finishConnect(r.Context(), n, pc, q, pc.person)
	if problem != "" {
		p.showConnectors(w, r, status, problem)
		return
	}
	p.tellOwner(r.Context(), n, connectorEmail(n, p.networkOrigin(r, n.Name)+"/connectors", "connected", title, inConsole(r), p.opts.Now()))
	http.Redirect(w, r, "/connectors", http.StatusSeeOther)
}

// disconnectHere forgets a service's connection, from the console.
func (p *Platform) disconnectHere(w http.ResponseWriter, r *http.Request) {
	if p.opts.Connections == nil {
		http.NotFound(w, r)
		return
	}
	n := networkOf(r)
	title, err := p.forgetConnection(r.Context(), n, r.PathValue("connector"))
	if err != nil && !errors.Is(err, ErrNotFound) {
		p.showConnectors(w, r, http.StatusInternalServerError, "Something went wrong. Try again in a moment.")
		return
	}
	if err == nil {
		p.tellOwner(r.Context(), n, connectorEmail(n, p.networkOrigin(r, n.Name)+"/connectors", "disconnected", title, inConsole(r), p.opts.Now()))
	}
	http.Redirect(w, r, "/connectors", http.StatusSeeOther)
}

// connectGitHubHere starts connecting GitHub, from the console: to GitHub's
// page for installing the app, with a state this browser alone will bring
// back.
func (p *Platform) connectGitHubHere(w http.ResponseWriter, r *http.Request) {
	g := p.opts.GitHub
	if g == nil {
		http.NotFound(w, r)
		return
	}
	n := networkOf(r)
	state := newToken()
	p.githubConnects.add(state, n.Name, p.opts.Now().Add(githubWindow))
	http.SetCookie(w, &http.Cookie{Name: p.githubCookie(), Value: state + "." + n.Name, Path: "/",
		MaxAge: int(githubWindow.Seconds()), HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, g.web()+"/apps/"+url.PathEscape(g.Slug)+"/installations/new?"+url.Values{"state": {state}}.Encode(), http.StatusSeeOther)
}

// connectedGitHubHere is GitHub sending the owner back, passed on by the
// home page: only in the browser that started it.
func (p *Platform) connectedGitHubHere(w http.ResponseWriter, r *http.Request) {
	g := p.opts.GitHub
	if g == nil {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: p.githubCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	n := networkOf(r)
	q := r.URL.Query()
	state, name := p.githubState(r)
	if state == "" || !strings.EqualFold(name, n.Name) || subtle.ConstantTimeCompare([]byte(state), []byte(q.Get("state"))) != 1 {
		p.showConnectors(w, r, http.StatusBadRequest, "That didn't start here, or took too long. Start connecting GitHub again.")
		return
	}
	p.githubConnects.remove(state)
	account, status, problem, err := p.bindGitHub(r.Context(), *g, n, q, byAccount(r))
	if problem != "" {
		if err != nil {
			monitor.Problem(p.opts.Logger, "connecting github: %v", err)
		}
		p.showConnectors(w, r, status, problem)
		return
	}
	p.tellOwner(r.Context(), n, githubEmail(n, p.networkOrigin(r, n.Name)+"/connectors", "connected", account, inConsole(r), p.opts.Now()))
	http.Redirect(w, r, "/connectors", http.StatusSeeOther)
}

// disconnectGitHubHere unbinds the network from its installation, from the
// console. (The app stays installed on GitHub until it's uninstalled there.)
func (p *Platform) disconnectGitHubHere(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	was, _ := p.opts.Networks.GitHubOf(r.Context(), n.Name)
	if err := p.opts.Networks.UnbindGitHub(r.Context(), n.Name); err != nil && !errors.Is(err, ErrNotFound) {
		monitor.Problem(p.opts.Logger, "disconnecting %s from github: %v", n.Name, err)
	} else if err == nil && was.Installation != 0 {
		p.tellOwner(r.Context(), n, githubEmail(n, p.networkOrigin(r, n.Name)+"/connectors", "disconnected", was.Account, inConsole(r), p.opts.Now()))
	}
	http.Redirect(w, r, "/connectors", http.StatusSeeOther)
}

// setConnectorTool sets a tool's policy, or approves it as it is now.
func (p *Platform) setConnectorTool(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if _, sent := r.PostForm["policy"]; sent {
		body["policy"] = strings.TrimSpace(r.PostFormValue("policy"))
	}
	if r.PostFormValue("approve") == "1" {
		body["approve"] = true
	}
	path := "/v1/connectors/" + url.PathEscape(r.PathValue("connector")) + "/tools/" + url.PathEscape(r.PathValue("tool"))
	if err := p.runner(r).call(r.Context(), "POST", path, body, nil); err != nil {
		p.showConnectors(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/connectors#"+url.PathEscape(r.PathValue("connector")), http.StatusSeeOther)
}
