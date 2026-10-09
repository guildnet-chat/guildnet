// SPDX-License-Identifier: AGPL-3.0-only

// Package platformtest runs GuildNet's console in tests, as people use it:
// a world of networks, each a real runner on a fake IRC server, and
// browsers that sign in and click through its pages. Extensions' tests
// plug theirs in (Setup) and drive it the same way.
//
// It's for tests only, and may change with the console.
package platformtest

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/ircfake"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/netspec"
	"github.com/guildnet-chat/guildnet/platform"
	"github.com/guildnet-chat/guildnet/runner"
)

// The console's tests drive the real runner and its API, over a fake
// IRC server, so what the console does goes through the same rules as
// GuildServ. Only Ergo's password check is faked.

// BaseDomain is the console's own address; networks are under it.
const BaseDomain = "guildnet.test"

// World is the platform with its networks.
type World struct {
	T        *testing.T
	Server   *httptest.Server
	Ergo     *FakeErgo
	Chat     *FakeChat
	Google   *FakeGoogle
	Host     *FakeHost
	Networks map[string]*Network
	Clock    *Clock

	Store   Store        // the platform's data, as first opened
	DBURL   string       // set when the platform keeps things in Postgres
	mu      sync.Mutex   // guards handler, and Broker as it changes
	handler http.Handler // the running platform; swapped by restartPlatform
	newPlat func() http.Handler
	admit   func(email string) // Setup.Admit, as last started
	Broker  http.Handler       // the running platform's GitHub token broker
	// Providers is where model providers are reached, to check keys.
	Providers map[models.Provider]string
	logs      *logs // what the console has logged
	// Stack is a self-hosted world's network (NewSelfHostedWorld).
	Stack *Stack
}

// logs is what a world's console logs, kept to be read.
type logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// Logged is what the console has logged so far.
func (w *World) Logged() string {
	w.logs.mu.Lock()
	defer w.logs.mu.Unlock()
	return w.logs.b.String()
}

// RestartPlatform starts a new platform on the same store, as a deploy does.
func (w *World) RestartPlatform() {
	h := w.newPlat()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handler = h
}

// Clock is the platform's time, which tests can move on.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now is the time on the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock on by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Network is one company's network: its IRC server, runner, and org.
type Network struct {
	Name   string
	IRC    *ircfake.Server
	Runner *runner.Runner
	Store  org.Store
	Brain  *StubBrain
	Ledger *ledger.Ledger
}

// acme's org: ada owns it; vera leads Engineering (kai is there too, and cy,
// a human who costs 6000/month); bo leads Sales.
func AcmeSeed() (string, org.Seed) {
	return "ada", org.Seed{
		Agents: []config.Persona{
			{Nick: "vera", Name: "Vera", Role: "Team lead", Team: "Engineering", ReportsTo: "ada", Model: "claude-opus-5", DailyBudgetUSD: 2, Channels: []string{"#general", "#engineering"}},
			{Nick: "kai", Name: "Kai", Role: "Backend engineer", Team: "Engineering", ReportsTo: "vera", Model: "claude-sonnet-5", DailyBudgetUSD: 1, Channels: []string{"#general", "#engineering"}},
		},
		Teams: []org.Team{
			{Name: "Engineering", Channel: "#engineering", Leads: []string{"vera"}, MonthlyBudget: 20000, Repos: []string{"web"}},
			{Name: "Sales", Channel: "#sales", Leads: []string{"bo"}, MonthlyBudget: 1000},
		},
		Members: []org.Member{
			{Account: "ada", Role: "Founder"},
			{Account: "bo", Role: "Head of Sales", Team: "Sales", ReportsTo: "ada"},
			{Account: "cy", Role: "Engineer", Team: "Engineering", ReportsTo: "vera", MonthlyCost: 6000},
		},
	}
}

// GlobexSeed is globex's org: gus owns it and leads Ops, where hal works.
func GlobexSeed() (string, org.Seed) {
	return "gus", org.Seed{
		Agents:  []config.Persona{{Nick: "hal", Name: "Hal", Role: "Operator", Team: "Ops", ReportsTo: "gus", Model: "claude-haiku-4-5", Channels: []string{"#general"}}},
		Teams:   []org.Team{{Name: "Ops", Channel: "#ops-team", Leads: []string{"gus"}}},
		Members: []org.Member{{Account: "gus", Role: "Founder"}},
	}
}

// Passwords each network's Ergo knows. zed has an account on acme's IRC
// server but isn't in the org.
var Passwords = map[string]map[string]string{
	"acme":   {"ada": "ada-pw", "bo": "bo-pw", "cy": "cy-pw", "zed": "zed-pw"},
	"globex": {"gus": "gus-pw"},
}

// ChatKeys are the keys each network's Ergo checks chat tokens with.
var ChatKeys = map[string]string{"acme": "acme-chat-key-0123456789abcdef", "globex": "globex-chat-key-0123456789abcdef"}

// FakeChat stands in for each network's Ergo WebSocket listener: it notes
// who connected and echoes every line back.
type FakeChat struct {
	Server *httptest.Server
	mu     sync.Mutex
	got    []ChatConn
}

// ChatConn is a connection to a network's Ergo WebSocket, and who it said
// it was for.
type ChatConn struct {
	Network, ForwardedFor string
}

func newFakeChat(t *testing.T) *FakeChat {
	f := &FakeChat{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Note the connection before answering it, so a test that has its
		// answer can count on it being here.
		f.mu.Lock()
		f.got = append(f.got, ChatConn{Network: strings.TrimPrefix(r.URL.Path, "/"), ForwardedFor: r.Header.Get("X-Forwarded-For")})
		f.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"text.ircv3.net"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, line, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), typ, line)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// Connections are the connections so far.
func (f *FakeChat) Connections() []ChatConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ChatConn(nil), f.got...)
}

// Store is what a world keeps the platform's data in.
type Store interface {
	platform.Networks
	platform.Sessions
	platform.People
	platform.PeopleAdmin
	platform.Links
	platform.Onboarding
	platform.Alerts
	platform.Connections
	platform.GitRepos
	platform.TwoSteps
	platform.HomeTwoSteps
	PutNetwork(ctx context.Context, n platform.Network) error
}

// NewWorld is the open console with acme's and globex's networks, keeping
// its data in memory.
func NewWorld(t *testing.T) *World {
	t.Helper()
	s := platform.NewMemoryStore()
	return NewWorldWith(t, func() Store { return s })
}

// NewWorldWithPostgres keeps the platform's data in a fresh Postgres
// database; each platform start opens its own connection to it.
func NewWorldWithPostgres(t *testing.T, tweaks ...func(*Setup)) *World {
	t.Helper()
	return NewWorldOn(t, NewDatabase(t), tweaks...)
}

// NewWorldOn keeps the platform's data in the Postgres database at url
// (from NewDatabase), which an extension's data may share.
func NewWorldOn(t *testing.T, url string, tweaks ...func(*Setup)) *World {
	t.Helper()
	w := NewWorldWith(t, func() Store {
		s, err := platform.OpenPostgres(context.Background(), url, MustSealer(t, TestKey))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}, tweaks...)
	w.DBURL = url
	return w
}

// Setup is how a world's console is set up, at each start: its options,
// with an extension plugged in if a test sets one (Options.Extension).
type Setup struct {
	platform.Options
	// Admit lets someone in before they sign in on the home page (OnHome):
	// onto an extension's list of who may use it, say. Nil means there's
	// nothing to be let into.
	Admit func(email string)
}

// NewWorldWith is the console with acme's and globex's networks, keeping
// its data in what open gives (each start, as a deploy does), set up by
// the tweaks. Without them it's the open console alone.
func NewWorldWith(t *testing.T, open func() Store, tweaks ...func(*Setup)) *World {
	t.Helper()
	return newWorld(t, open, []string{"acme", "globex"}, nil, tweaks...)
}

// seeds are the networks a world can start with, by name.
var seeds = map[string]func() (string, org.Seed){"acme": AcmeSeed, "globex": GlobexSeed}

// newWorld is the console with the networks named, keeping its data in
// what open gives, set up by pre (if any) and then the tweaks.
func newWorld(t *testing.T, open func() Store, networks []string, pre func(*World, *Setup), tweaks ...func(*Setup)) *World {
	t.Helper()
	w := &World{T: t, Networks: map[string]*Network{}, Clock: &Clock{now: time.Now()}, logs: &logs{}}
	w.Ergo = newFakeErgo(t)
	w.Providers = NewFakeModelProviders(t)
	w.Chat = newFakeChat(t)
	w.Google = newFakeGoogle(t)
	w.Host = &FakeHost{w: w, Specs: map[string]netspec.Spec{}, Running: map[string]netspec.Addresses{}, Stopped: map[string]bool{}}
	first := open()
	w.Store = first
	for _, name := range networks {
		owner, s := seeds[name]()
		n := StartTestNetwork(t, name, owner, s)
		n.Runner.UseGitRepos(storeGitRepos{store: first, network: name})
		w.Networks[name] = n
		err := first.AddNetwork(context.Background(), platform.Network{
			Name: name, Region: "us-west1",
			RunnerURL: "http://" + n.Runner.APIAddr(), RunnerToken: name + "-runner-secret",
			ErgoURL: w.Ergo.Server.URL + "/" + name, ErgoToken: name + "-ergo-secret",
			ChatURL: "ws" + strings.TrimPrefix(w.Chat.Server.URL, "http") + "/" + name, ChatKey: ChatKeys[name],
			Status: platform.Ready,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	started := false
	w.newPlat = func() http.Handler {
		s := first
		if started {
			s = open()
		}
		started = true
		o := Setup{
			Options: platform.Options{BaseDomain: BaseDomain, Networks: s, Sessions: s, People: s, Links: s, Onboarding: s, Alerts: s, Connections: s, GitRepos: s, TwoSteps: s, HomeTwoSteps: s,
				Google: w.Google.Options(), Host: w.Host, Region: "us-west1", ModelProviders: w.Providers,
				Logger: log.New(w.logs, "", 0), Now: w.Clock.Now},
		}
		if pre != nil {
			pre(w, &o)
		}
		for _, tweak := range tweaks {
			tweak(&o)
		}
		w.admit = o.Admit
		p := platform.New(o.Options)
		broker := p.Broker()
		w.mu.Lock()
		w.Broker = broker
		w.mu.Unlock()
		return p.Handler()
	}
	w.handler = w.newPlat()
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		h := w.handler
		w.mu.Unlock()
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(w.Server.Close)
	// A page that couldn't be rendered was cut short, whatever it showed.
	t.Cleanup(func() {
		for _, line := range strings.Split(w.Logged(), "\n") {
			if strings.Contains(line, "rendering ") {
				t.Errorf("the console couldn't render a page: %s", line)
			}
		}
	})
	return w
}

// AnyoneCreates is a test extension that lets anyone signed in on the
// home page create networks (the open console lets only whoever sets it
// up create one), for tests of what happens with several.
func AnyoneCreates(o *Setup) {
	o.Extension = platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
		return platform.Hooks{MayCreate: func(context.Context, platform.Person, int) (platform.CreateAnswer, error) {
			return platform.CreateAnswer{}, nil
		}}
	})
}

// StartTestNetwork starts a network's real runner, with org seed, on a fake
// IRC server.
func StartTestNetwork(t *testing.T, name, owner string, seed org.Seed) *Network {
	t.Helper()
	irc, err := ircfake.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(irc.Close)
	cfg := &config.Runner{
		Server: irc.Addr(), Owner: owner, Channels: []string{"#general"}, OpsChannel: "#ops", LedgerNick: "ledger",
		DailyBudgetUSD: 5, LoopLimit: 6, HistoryLines: 40, DataDir: t.TempDir(),
		GeneralChannel: "#general", AnnouncementsChannel: "#announcements", FeedbackChannel: "#feedback", Location: time.UTC, ContinueWithin: -1,
	}
	cfg.API.Listen, cfg.API.Token = "127.0.0.1:0", name+"-runner-secret"
	cfg.GitHub.Owner, cfg.GitHub.Repos = name+"-inc", []string{"web", "docs"} // what its GitHub installation has
	l, err := ledger.Open(cfg.DataDir, cfg.DailyBudgetUSD)
	if err != nil {
		t.Fatal(err)
	}
	// Before its directory goes: work still finishing writes nothing there.
	t.Cleanup(l.Close)
	store := org.NewMemory(seed)
	brain := &StubBrain{}
	r := runner.New(cfg, store, brain, l, log.New(io.Discard, "", 0))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	return &Network{Name: name, IRC: irc, Runner: r, Store: store, Brain: brain, Ledger: l}
}

// RaiseApproval puts an approval in front of an approver, as an agent
// pausing before a risky action does.
func (n *Network) RaiseApproval(t *testing.T, agentNick, approver, summary string) int64 {
	t.Helper()
	now := time.Now()
	a, err := n.Store.RaiseApproval(context.Background(), org.Approval{Agent: agentNick, Approver: approver, Tool: "github_merge_pull_request",
		Summary: summary, Channel: "#engineering", Asker: approver, Resume: "token", Status: org.Pending, RaisedAt: now, ExpiresAt: now.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// RaiseOwnWordsApproval puts an outside agent's approval in its own words
// (request_approval: nothing pinned to it) in front of an approver.
func (n *Network) RaiseOwnWordsApproval(t *testing.T, agentNick, approver, summary string) int64 {
	t.Helper()
	now := time.Now()
	a, err := n.Store.RaiseApproval(context.Background(), org.Approval{Agent: agentNick, Approver: approver, Tool: "request_approval",
		Summary: summary, Channel: "#engineering", Asker: agentNick, Resume: `{"outside":true}`, Status: org.Pending, RaisedAt: now, ExpiresAt: now.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// StubBrain stands in for Claude: agents never say anything; it records
// approval decisions.
type StubBrain struct {
	mu        sync.Mutex
	resumes   []string // "agent allow|deny: reason"
	platforms []string // where agents can work besides chat ("desk", "managed")
}

// Offer lets agents here work on platform too.
func (b *StubBrain) Offer(platform string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.platforms = append(b.platforms, platform)
}

func (b *StubBrain) HasPlatform(platform string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return platform == "" || platform == "chat" || slices.Contains(b.platforms, platform)
}

func (b *StubBrain) Respond(context.Context, agent.Request, func(agent.Step)) (agent.Result, error) {
	return agent.Result{Reply: agent.Pass}, nil
}

func (b *StubBrain) Resume(_ context.Context, p config.Persona, _ string, allow bool, reason string, _ func(agent.Step)) (agent.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	verdict := "deny"
	if allow {
		verdict = "allow"
	}
	b.resumes = append(b.resumes, p.Nick+" "+verdict+": "+reason)
	return agent.Result{Reply: agent.Pass}, nil
}

func (b *StubBrain) PickResponders(context.Context, string, string, string, []chat.Line, []config.Persona) ([]string, ledger.Usage, error) {
	return nil, ledger.Usage{}, nil
}

func (b *StubBrain) Catchup(context.Context, agent.CatchupRequest) (string, ledger.Usage, error) {
	return "", ledger.Usage{}, nil
}

// Resumed are the approval decisions agents resumed with: "agent
// allow|deny: reason".
func (b *StubBrain) Resumed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.resumes...)
}

// FakeErgo is each network's Ergo HTTP API: its status, its password check,
// and making accounts. Each network has its own API token, and a call with
// another's gets nothing.
type FakeErgo struct {
	Server   *httptest.Server
	mu       sync.Mutex
	tokens   map[string]string            // network -> API token
	accounts map[string]map[string]string // network -> account -> password
}

func newFakeErgo(t *testing.T) *FakeErgo {
	f := &FakeErgo{tokens: map[string]string{}, accounts: map[string]map[string]string{}}
	for network, accounts := range Passwords {
		f.tokens[network] = network + "-ergo-secret"
		f.accounts[network] = map[string]string{}
		for a, pw := range accounts {
			f.accounts[network][a] = pw
		}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		token, ok := f.tokens[network]
		if r.Method != "POST" || !ok || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		var body struct {
			AccountName, Passphrase string
		}
		json.NewDecoder(r.Body).Decode(&body)
		// As Ergo does: no spaces, and no colon first (see ergotest).
		if (path == "v1/ns/saregister" || path == "v1/ns/passwd") &&
			(strings.ContainsFunc(body.Passphrase, unicode.IsSpace) || strings.HasPrefix(body.Passphrase, ":") || body.Passphrase == "") {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errorCode": "INVALID_PASSPHRASE"})
			return
		}
		switch path {
		case "v1/status":
			// As Ergo answers: who's connected (here, everyone with an
			// account), and how many channels there are.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "users": map[string]int{"total": len(f.accounts[network])}, "channels": 5})
		case "v1/check_auth":
			pw, ok := f.accounts[network][body.AccountName]
			json.NewEncoder(w).Encode(map[string]bool{"success": ok && pw == body.Passphrase})
		case "v1/ns/saregister":
			if _, taken := f.accounts[network][body.AccountName]; taken {
				json.NewEncoder(w).Encode(map[string]any{"success": false, "errorCode": "ACCOUNT_EXISTS"})
				return
			}
			f.accounts[network][body.AccountName] = body.Passphrase
			json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case "v1/ns/passwd":
			if _, ok := f.accounts[network][body.AccountName]; !ok {
				json.NewEncoder(w).Encode(map[string]any{"success": false, "errorCode": "ACCOUNT_DOES_NOT_EXIST"})
				return
			}
			f.accounts[network][body.AccountName] = body.Passphrase
			json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// AddNetwork gives a new network's Ergo its API token.
func (f *FakeErgo) AddNetwork(network, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[network] = token
	if f.accounts[network] == nil {
		f.accounts[network] = map[string]string{}
	}
}

// AddAccount makes an account on network's Ergo.
func (f *FakeErgo) AddAccount(network, account, password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[network][account] = password
}

// Password is account's password on network's Ergo, if it has one.
func (f *FakeErgo) Password(network, account string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pw, ok := f.accounts[network][account]
	return pw, ok
}

// Browser is a person's browser on one network's address: it keeps that
// address's cookies (and only those), and sends what browsers send.
type Browser struct {
	T       *testing.T
	W       *World
	Host    string
	Cookies map[string]*http.Cookie
	Last    string // the last page's body
	Origin  string // the Origin sent with changes; "" means this host's own
	// ForwardedFor is sent as X-Forwarded-For, as a proxy in front would.
	ForwardedFor string
}

// Browser is a browser on network's address.
func (w *World) Browser(network string) *Browser {
	return &Browser{T: w.T, W: w, Host: network + "." + BaseDomain, Cookies: map[string]*http.Cookie{}}
}

// Response is what came back.
type Response struct {
	Status int
	Body   string
	Header http.Header
}

// Do sends a request, with the browser's cookies, and keeps what comes
// back.
func (b *Browser) Do(method, path string, form url.Values) Response {
	b.T.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, b.W.Server.URL+path, body)
	if err != nil {
		b.T.Fatal(err)
	}
	req.Host = b.Host
	if b.ForwardedFor != "" {
		req.Header.Set("X-Forwarded-For", b.ForwardedFor)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != "GET" {
		origin := b.Origin
		if origin == "" {
			origin = "http://" + b.Host
		}
		req.Header.Set("Origin", origin)
	}
	for _, c := range b.Cookies {
		req.AddCookie(c)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		b.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.Cookies, c.Name)
		} else {
			b.Cookies[c.Name] = c
		}
	}
	b.Last = string(raw)
	return Response{Status: resp.StatusCode, Body: string(raw), Header: resp.Header}
}

// Get asks for a page.
func (b *Browser) Get(path string) Response { return b.Do("GET", path, nil) }

// Text is the page as a person reads it, with HTML's escapes undone.
func (r Response) Text() string { return html.UnescapeString(r.Body) }

// Post submits a form, with the page's CSRF token, as a browser submitting
// a form on the last page would.
func (b *Browser) Post(path string, form url.Values) Response {
	b.T.Helper()
	if form == nil {
		form = url.Values{}
	}
	if form.Get("csrf") == "" {
		form.Set("csrf", b.CSRF())
	}
	return b.Do("POST", path, form)
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// CSRF is the CSRF token on the last page, if any.
func (b *Browser) CSRF() string {
	if m := csrfField.FindStringSubmatch(b.Last); m != nil {
		return m[1]
	}
	return ""
}

// SignIn signs in to the network's console with an IRC account.
func (b *Browser) SignIn(account, password string) Response {
	b.T.Helper()
	b.Get("/signin")
	return b.Post("/signin", url.Values{"account": {account}, "password": {password}})
}

// SignedIn signs in and fails the test if that didn't work.
func (b *Browser) SignedIn(account string) *Browser {
	b.T.Helper()
	network := strings.TrimSuffix(b.Host, "."+BaseDomain)
	if r := b.SignIn(account, Passwords[network][account]); r.Status != http.StatusSeeOther {
		b.T.Fatalf("signing in as %s: %d %s", account, r.Status, r.Body)
	}
	return b
}
