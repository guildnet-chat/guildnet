// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/internal/github"
	"github.com/guildnet-chat/guildnet/monitor"
)

// GitHub for customers. One public GitHub App, GuildNet's, that any account
// can install. Its private key and client secret stay here, on the
// platform: the key can mint tokens for every customer's installation, so
// it never goes to a network's runner. Instead:
//
//   - A network's owner connects it from the home page, signed in with
//     Google as its owner. GitHub sends them back here with the installation
//     they chose, and it's bound to the network only if their own GitHub
//     account can reach it (the installation's id arrives in the address,
//     where anyone could type someone else's), and only if it isn't bound to
//     another network.
//   - The network's runner asks the broker, on the platform's internal
//     listener and with the network's secret, for tokens: only for its own
//     installation, only for the repos it names, read-only or with what the
//     tools need.

// GitHubApp is GuildNet's public GitHub App.
type GitHubApp struct {
	AppID        int64
	Slug         string // its address on GitHub: github.com/apps/<slug>
	ClientID     string // for checking who's connecting
	ClientSecret string
	Key          []byte // its private key, PEM
	APIURL       string // the API; empty means https://api.github.com
	WebURL       string // GitHub itself; empty means https://github.com
}

func (g GitHubApp) api() string {
	if g.APIURL != "" {
		return strings.TrimRight(g.APIURL, "/")
	}
	return "https://api.github.com"
}

func (g GitHubApp) web() string {
	if g.WebURL != "" {
		return strings.TrimRight(g.WebURL, "/")
	}
	return "https://github.com"
}

// GitHubLink is a network's GitHub installation.
type GitHubLink struct {
	Installation int64
	Account      string // the GitHub account it's installed on
	By           string // who connected it (a Person's ID)
	At           time.Time
}

// ErrGitHubTaken means the installation is connected to another network.
var ErrGitHubTaken = errors.New("that GitHub installation is connected to another network")

// githubWindow is how long connecting may take on GitHub's side.
const githubWindow = 10 * time.Minute

// consoleGitHubConnects are connects started in networks' consoles, by
// their states: which network, so the home page's callback can pass each
// on to its console.
type consoleGitHubConnects struct {
	mu sync.Mutex
	m  map[string]pendingGitHub // by the state's hash
}

type pendingGitHub struct {
	network string
	expires time.Time
}

func (c *consoleGitHubConnects) add(state, network string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]pendingGitHub{}
	}
	for k, x := range c.m {
		if time.Now().After(x.expires) {
			delete(c.m, k)
		}
	}
	c.m[hashToken(state)] = pendingGitHub{network: network, expires: expires}
}

func (c *consoleGitHubConnects) peek(state string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.m[hashToken(state)]
	return x.network, ok && state != "" && now.Before(x.expires)
}

func (c *consoleGitHubConnects) remove(state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, hashToken(state))
}

func (p *Platform) githubCookie() string {
	if p.opts.SecureCookies {
		return "__Host-guildnet-github"
	}
	return "guildnet-github"
}

// connectGitHub starts connecting the owner's network: to GitHub's page for
// installing the app, with a state this browser alone will bring back.
func (p *Platform) connectGitHub(w http.ResponseWriter, r *http.Request) {
	g := p.opts.GitHub
	if g == nil {
		http.NotFound(w, r)
		return
	}
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	state := newToken()
	http.SetCookie(w, &http.Cookie{Name: p.githubCookie(), Value: state + "." + n.Name, Path: "/",
		MaxAge: int(githubWindow.Seconds()), HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, g.web()+"/apps/"+url.PathEscape(g.Slug)+"/installations/new?"+url.Values{"state": {state}}.Encode(), http.StatusSeeOther)
}

// githubCallback is GitHub sending the owner back after installing the app.
// One that started in a network's console goes on there.
func (p *Platform) githubCallback(w http.ResponseWriter, r *http.Request) {
	g := p.opts.GitHub
	if g == nil {
		http.NotFound(w, r)
		return
	}
	if network, ok := p.githubConnects.peek(r.URL.Query().Get("state"), p.opts.Now()); ok {
		http.Redirect(w, r, p.networkOrigin(r, network)+"/github/callback?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}
	// The connect in progress is used up either way.
	http.SetCookie(w, &http.Cookie{Name: p.githubCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	fail := func(status int, msg string, err error) {
		if err != nil {
			monitor.Problem(p.opts.Logger, "connecting github: %v", err)
		}
		p.renderHome(w, r, status, "home-signin", withError{Err: msg})
	}
	s, signedIn := p.loadHomeSession(r)
	state, name := p.githubState(r)
	q := r.URL.Query()
	if !signedIn || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(q.Get("state"))) != 1 {
		fail(http.StatusBadRequest, "That didn't start here, or took too long. Start connecting GitHub again from your network's page.", nil)
		return
	}
	n, err := p.opts.Networks.Network(r.Context(), name)
	if err != nil || n.Owner != s.person.ID {
		fail(http.StatusForbidden, "Only a network's owner can connect it to GitHub.", nil)
		return
	}
	account, status, problem, err := p.bindGitHub(r.Context(), *g, n, q, s.person.ID)
	if problem != "" {
		fail(status, problem, err)
		return
	}
	p.tellOwner(r.Context(), n, githubEmail(n, p.homeURL(r, "/networks/"+n.Name), "connected", account, p.onHome(s.person.Email), p.opts.Now()))
	http.Redirect(w, r, "/networks/"+url.PathEscape(n.Name), http.StatusSeeOther)
}

// githubState is the connect this browser started: its state, and the
// network's name.
func (p *Platform) githubState(r *http.Request) (string, string) {
	c, err := r.Cookie(p.githubCookie())
	if err != nil {
		return "", ""
	}
	state, name, _ := strings.Cut(c.Value, ".")
	return state, name
}

// bindGitHub binds n to the installation GitHub sent back (q), as by, if
// the person's own GitHub account can reach it: the account it's on. If it
// isn't bound, what's said is why, with its status.
func (p *Platform) bindGitHub(ctx context.Context, g GitHubApp, n Network, q url.Values, by string) (account string, status int, problem string, err error) {
	installation, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
	if err != nil || q.Get("code") == "" {
		return "", http.StatusBadRequest, "GitHub didn't say which installation. Try connecting again.", nil
	}
	token, err := p.githubUserToken(ctx, g, q.Get("code"))
	if err != nil {
		return "", http.StatusBadGateway, "Couldn't check with GitHub. Try connecting again.", err
	}
	account, ok, err := p.githubReaches(ctx, g, token, installation)
	switch {
	case err != nil:
		return "", http.StatusBadGateway, "Couldn't check with GitHub. Try connecting again.", err
	case !ok:
		return "", http.StatusForbidden, "Only an admin of the GitHub account it's installed on, with admin on all its repos, can connect that installation, so it wasn't connected.", nil
	}
	err = p.opts.Networks.BindGitHub(ctx, n.Name, GitHubLink{Installation: installation, Account: account, By: by, At: p.opts.Now()})
	if errors.Is(err, ErrGitHubTaken) {
		return "", http.StatusConflict, "That GitHub installation is already connected to another network.", nil
	}
	if err != nil {
		return "", http.StatusInternalServerError, "Couldn't save the connection. Try again.", err
	}
	p.opts.Logger.Printf("%s connected to github %s (installation %d)", n.Name, account, installation)
	return account, http.StatusOK, "", nil
}

// disconnectGitHub unbinds the owner's network from its installation. (The
// app stays installed on GitHub until they uninstall it there.)
func (p *Platform) disconnectGitHub(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	was, _ := p.opts.Networks.GitHubOf(r.Context(), n.Name)
	if err := p.opts.Networks.UnbindGitHub(r.Context(), n.Name); err != nil && !errors.Is(err, ErrNotFound) {
		monitor.Problem(p.opts.Logger, "disconnecting %s from github: %v", n.Name, err)
	} else if err == nil && was.Installation != 0 {
		p.tellOwner(r.Context(), n, githubEmail(n, p.homeURL(r, "/networks/"+n.Name), "disconnected", was.Account, p.onHome(homeOf(r).person.Email), p.opts.Now()))
	}
	http.Redirect(w, r, "/networks/"+url.PathEscape(n.Name), http.StatusSeeOther)
}

// githubUserToken trades the code GitHub sent back for the person's own
// token, used only to check what they can reach, then forgotten.
func (p *Platform) githubUserToken(ctx context.Context, g GitHubApp, code string) (string, error) {
	form := url.Values{"client_id": {g.ClientID}, "client_secret": {g.ClientSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, "POST", g.web()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("no token (%d %s)", resp.StatusCode, out.Error)
	}
	return out.AccessToken, nil
}

// githubReaches says whether the person with token may connect
// installation, and the account it's installed on: they must see it, and
// be an admin of every repo it gives GuildNet (as an organization's admins
// are, and a person of their own account's). GitHub lists an installation
// for anyone who can read one of its repos, and the network gets the app's
// write to all of them. (Asked through the installation, which the app may
// always do; an organization's membership would need a permission more.)
func (p *Platform) githubReaches(ctx context.Context, g GitHubApp, token string, installation int64) (string, bool, error) {
	account, kind, seen, err := p.githubSees(ctx, g, token, installation)
	if err != nil || !seen {
		return "", false, err
	}
	all, err := p.freshInstallationRepos(ctx, installation)
	if err != nil {
		return "", false, err
	}
	if len(all) == 0 {
		// Nothing to be an admin of yet: only the person whose own account
		// it's on.
		if kind != "User" {
			return account, false, nil
		}
		var me struct {
			Login string `json:"login"`
		}
		if status, err := githubGet(ctx, g, token, "/user", &me); err != nil || status != http.StatusOK {
			return "", false, fmt.Errorf("who's signed in: %d %v", status, err)
		}
		return account, strings.EqualFold(me.Login, account), nil
	}
	admin := map[string]bool{}
	for page := 1; page <= 10; page++ {
		var out struct {
			Repositories []struct {
				Name        string `json:"name"`
				Permissions struct {
					Admin bool `json:"admin"`
				} `json:"permissions"`
			} `json:"repositories"`
		}
		status, err := githubGet(ctx, g, token, fmt.Sprintf("/user/installations/%d/repositories?per_page=100&page=%d", installation, page), &out)
		if err != nil || status != http.StatusOK {
			return "", false, fmt.Errorf("listing their repos: %d %v", status, err)
		}
		for _, r := range out.Repositories {
			admin[r.Name] = r.Permissions.Admin
		}
		if len(out.Repositories) < 100 {
			break
		}
	}
	for _, repo := range all {
		if !admin[repo] {
			return account, false, nil
		}
	}
	return account, true, nil
}

// githubGet gets path from GitHub's API with the person's token, into out.
func githubGet(ctx context.Context, g GitHubApp, token, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", g.api()+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return resp.StatusCode, nil
}

// githubSees says whether the person with token sees installation among
// theirs, and the account it's on, and that account's kind (User or
// Organization).
func (p *Platform) githubSees(ctx context.Context, g GitHubApp, token string, installation int64) (string, string, bool, error) {
	for page := 1; page <= 10; page++ {
		var out struct {
			Installations []struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"account"`
			} `json:"installations"`
		}
		status, err := githubGet(ctx, g, token, fmt.Sprintf("/user/installations?per_page=100&page=%d", page), &out)
		if err != nil || status != http.StatusOK {
			return "", "", false, fmt.Errorf("listing installations: %d %v", status, err)
		}
		for _, i := range out.Installations {
			if i.ID == installation {
				return i.Account.Login, i.Account.Type, true, nil
			}
		}
		if len(out.Installations) < 100 {
			break
		}
	}
	return "", "", false, nil
}

// The broker.

// Broker is where networks' runners get what they need from the platform:
// their model providers' keys, and GitHub tokens. It's on the platform's
// internal listener, never the internet. A runner names its network and
// carries its secret, and only ever gets its own network's.
func (p *Platform) Broker() http.Handler {
	mux := http.NewServeMux()
	// The keys go only to the runner's memory: never to the host, its disk,
	// or the runner's settings. Not cached along the way.
	mux.HandleFunc("GET /model-keys", func(w http.ResponseWriter, r *http.Request) {
		n, ok := p.runnerCalling(r)
		if !ok {
			brokerError(w, http.StatusUnauthorized, "the network's secret is required")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSONResponse(w, http.StatusOK, map[string]any{"keys": n.Keys.Map()})
	})
	// A self-hosted network's settings, once first-run setup has made it.
	mux.HandleFunc("GET /settings", p.networkSettings)
	// How someone signs in to the console, for GuildServ: two-step accounts
	// change the org only from here.
	mux.HandleFunc("GET /two-step", func(w http.ResponseWriter, r *http.Request) {
		n, ok := p.runnerCalling(r)
		if !ok {
			brokerError(w, http.StatusUnauthorized, "the network's secret is required")
			return
		}
		account := r.URL.Query().Get("account")
		var on bool
		if p.opts.TwoSteps != nil && account != "" {
			t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, account)
			if err != nil {
				monitor.Problem(p.opts.Logger, "%s's two-step on %s: %v", account, n.Name, err)
				brokerError(w, http.StatusInternalServerError, "couldn't check")
				return
			}
			on = t.On()
		}
		writeJSONResponse(w, http.StatusOK, map[string]bool{"required": p.opts.RequireTwoStep && p.opts.TwoSteps != nil, "on": on})
	})
	mux.HandleFunc("GET /github", p.brokered(func(w http.ResponseWriter, r *http.Request, n Network, link GitHubLink) {
		repos, err := p.installationRepos(r.Context(), link.Installation)
		if err != nil {
			monitor.Problem(p.opts.Logger, "listing %s's github repos: %v", n.Name, err)
			brokerError(w, http.StatusBadGateway, "couldn't list the installation's repos")
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"account": link.Account, "repos": repos})
	}))
	mux.HandleFunc("POST /github/token", p.brokered(func(w http.ResponseWriter, r *http.Request, n Network, link GitHubLink) {
		var in struct {
			Repos []string `json:"repos"`
			Write bool     `json:"write"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || len(in.Repos) == 0 {
			brokerError(w, http.StatusBadRequest, "say which repos")
			return
		}
		token, expires, err := p.githubApp.Token(r.Context(), link.Installation, in.Repos, in.Write)
		if err != nil {
			monitor.Problem(p.opts.Logger, "minting a github token for %s: %v", n.Name, err)
			brokerError(w, http.StatusBadGateway, "GitHub didn't give a token")
			return
		}
		p.opts.Logger.Printf("github token for %s: %s, write %v", n.Name, strings.Join(in.Repos, ","), in.Write)
		writeJSONResponse(w, http.StatusOK, map[string]any{"token": token, "expires_at": expires})
	}))
	// Connectors: the network's connections, and access tokens for them.
	mux.HandleFunc("GET /connectors", p.brokerConnectors)
	mux.HandleFunc("POST /connectors/{connector}/token", p.brokerConnectorToken)
	// Git repositories on any host, and a credential to read or push with.
	mux.HandleFunc("GET /git", p.brokerGitRepos)
	mux.HandleFunc("POST /git/{name}/{access}", p.brokerGitCredential)
	// What happened on the network that people may want by email.
	mux.HandleFunc("POST /events", p.news)
	// Desks of GuildNet's own: where sandboxd is, with the network's token;
	// and waking the sandbox VM when it's asleep.
	mux.HandleFunc("GET /sandbox", p.brokerSandbox)
	mux.HandleFunc("POST /sandbox/wake", p.brokerSandboxWake)
	// Desk time: whether a desk may work, and how long it did.
	if p.hooks.Desks != nil {
		mux.HandleFunc("POST /desks/start", p.brokerDeskStart)
		mux.HandleFunc("POST /desks/done", p.brokerDeskDone)
	}
	return mux
}

// brokered checks a runner's call: its network, its secret, and that the
// network is connected to GitHub.
func (p *Platform) brokered(h func(http.ResponseWriter, *http.Request, Network, GitHubLink)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.githubApp == nil {
			brokerError(w, http.StatusNotFound, "GitHub isn't set up here")
			return
		}
		n, ok := p.runnerCalling(r)
		if !ok {
			brokerError(w, http.StatusUnauthorized, "the network's secret is required")
			return
		}
		link, err := p.opts.Networks.GitHubOf(r.Context(), n.Name)
		if err != nil || link.Installation == 0 {
			brokerError(w, http.StatusNotFound, "this network isn't connected to GitHub")
			return
		}
		h(w, r, n, link)
	}
}

// runnerCalling is the network whose runner is calling: the one it names,
// if it carries that network's secret.
func (p *Platform) runnerCalling(r *http.Request) (Network, bool) {
	n, err := p.opts.Networks.Network(r.Context(), r.Header.Get("GuildNet-Network"))
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err != nil || n.RunnerToken == "" || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(n.RunnerToken)) != 1 {
		return Network{}, false
	}
	// A network that's suspended, or being deleted, gets nothing more: its
	// runner may still be up, or its secret held by someone.
	if n.Status == Suspended || n.Status == Deleting {
		return Network{}, false
	}
	return n, true
}

// reposKept is how long an installation's repos are kept between asking
// GitHub: runners ask every couple of minutes, and a token's minted each
// time GitHub is asked.
const reposKept = 5 * time.Minute

// installationRepos are the repos an installation gives GuildNet.
func (p *Platform) installationRepos(ctx context.Context, installation int64) ([]string, error) {
	now := p.opts.Now()
	p.repos.mu.Lock()
	kept, ok := p.repos.m[installation]
	p.repos.mu.Unlock()
	if ok && now.Sub(kept.at) < reposKept {
		return slices.Clone(kept.repos), nil
	}
	return p.freshInstallationRepos(ctx, installation)
}

// freshInstallationRepos asks GitHub for an installation's repos now, and
// keeps them for next time.
func (p *Platform) freshInstallationRepos(ctx context.Context, installation int64) ([]string, error) {
	now := p.opts.Now()
	repos, err := p.fetchInstallationRepos(ctx, installation)
	if err != nil {
		return nil, err
	}
	p.repos.mu.Lock()
	if p.repos.m == nil {
		p.repos.m = map[int64]keptRepos{}
	}
	p.repos.m[installation] = keptRepos{repos: slices.Clone(repos), at: now}
	p.repos.mu.Unlock()
	return repos, nil
}

type keptRepos struct {
	repos []string
	at    time.Time
}

// reposByInstallation are installations' repos, as GitHub last said.
type reposByInstallation struct {
	mu sync.Mutex
	m  map[int64]keptRepos
}

func (p *Platform) fetchInstallationRepos(ctx context.Context, installation int64) ([]string, error) {
	token, _, err := p.githubApp.Token(ctx, installation, nil, false)
	if err != nil {
		return nil, err
	}
	var repos []string
	for page := 1; page <= 10; page++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", p.opts.GitHub.api(), page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		var out struct {
			Repositories []struct {
				Name string `json:"name"`
			} `json:"repositories"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing repos: %d %v", resp.StatusCode, err)
		}
		for _, r := range out.Repositories {
			repos = append(repos, r.Name)
		}
		if len(out.Repositories) < 100 {
			break
		}
	}
	slices.Sort(repos)
	return repos, nil
}

func brokerError(w http.ResponseWriter, status int, msg string) {
	writeJSONResponse(w, status, map[string]string{"error": msg})
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// newGitHubApp is the app's token minting, when the app is set up.
func newGitHubApp(g *GitHubApp) (*github.App, error) {
	if g == nil {
		return nil, nil
	}
	return github.NewApp(g.AppID, g.Key, g.APIURL)
}
