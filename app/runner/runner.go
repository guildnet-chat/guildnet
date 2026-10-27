// SPDX-License-Identifier: AGPL-3.0-only

// Package app runs a network's runner, which connects its agents to its IRC
// network, from its settings (the environment and guildnet.yaml): what the
// open build's command is, and what a build with more plugged in
// (GuildNet's model gateway) starts from.
package app

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/brain"
	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/github"
	"github.com/guildnet-chat/guildnet/internal/gitremote"
	"github.com/guildnet-chat/guildnet/internal/managed"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/pki"
	"github.com/guildnet-chat/guildnet/runner"
)

// Models is how agents outside the network call models on its keys
// (GuildNet's model gateway).
type Models interface {
	// SetKeys gives it the network's keys when they change.
	SetKeys(keys map[models.Provider]string)
	// Handler serves the calls, for the runner.
	Handler(c *runner.Core) http.Handler
}

// Config is what a build plugs into the runner; its zero value is the
// runner on its own.
type Config struct {
	// Models makes how agents outside call models, from the network's keys;
	// nil means they bring their own.
	Models func(keys map[models.Provider]string) Models
}

// Run connects the network's agents to its IRC network, as its settings
// say, until it's told to stop.
func Run(build Config) {
	// A network made from a template is pointed at it by GUILDNET_CONFIG
	// (e.g. /app/templates/team/guildnet.yaml).
	defaultConfig := "guildnet.yaml"
	if s := os.Getenv("GUILDNET_CONFIG"); s != "" {
		defaultConfig = s
	}
	cfgPath := flag.String("config", defaultConfig, "path to the runner config")
	flag.Parse()
	logger := log.New(os.Stdout, "", log.LstdFlags)
	// Faults in GuildNet itself go to GuildNet's Sentry, when it's set.
	defer monitor.Start(monitor.FromEnv("runner"))()
	defer monitor.Recover()

	// Model keys: from the platform's broker, kept only in memory (never on
	// the host's disk or in the container's settings); on a developer's
	// machine, from the environment.
	var keys map[models.Provider]string
	var keyBroker *brain.Broker
	brokerHTTP, err := brokerClient(os.Getenv)
	if err != nil {
		logger.Fatal(err)
	}
	if url := os.Getenv("GUILDNET_KEYS_BROKER"); url != "" {
		keyBroker = &brain.Broker{URL: url, Network: os.Getenv("GUILDNET_NETWORK"), Secret: os.Getenv("GUILDNET_API_TOKEN"), HTTP: brokerHTTP}
	}
	// A self-hosted network's runner waits for first-run setup, then starts
	// from the template it chose, with its owner, company and timezone. When
	// they change, it restarts (its stack starts it again) to take them up.
	var settings *networkSettings
	restart := make(chan struct{}, 1)
	if os.Getenv("GUILDNET_SETTINGS_FROM_BROKER") == "on" {
		if keyBroker == nil {
			logger.Fatal("GUILDNET_SETTINGS_FROM_BROKER needs GUILDNET_KEYS_BROKER: the network's settings come from the console's broker")
		}
		s, err := waitForSettings(context.Background(), keyBroker, 3*time.Second, logger)
		if err != nil {
			logger.Fatal(err)
		}
		dir := os.Getenv("GUILDNET_TEMPLATES")
		if dir == "" {
			dir = "/app/templates"
		}
		if *cfgPath, err = s.configPath(dir); err != nil {
			logger.Fatal(err)
		}
		for k, v := range s.env() {
			os.Setenv(k, v)
		}
		settings = &s
		logger.Printf("network settings from the console: template %s, owned by %s", s.Template, s.Owner)
	}
	if keyBroker != nil {
		// The platform may be restarting too (a deploy): keep asking a while.
		for try := 1; ; try++ {
			k, err := keyBroker.Keys(context.Background())
			if err == nil {
				keys = k
				break
			}
			if try == 20 {
				logger.Printf("no model keys from the broker yet (%v); starting, and asking again", err)
				break
			}
			time.Sleep(3 * time.Second)
		}
	} else if keys = brain.KeysFromEnv(); len(keys) == 0 {
		logger.Fatal("no model provider's key is set: set ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY or OPENROUTER_API_KEY, then open a new terminal.")
	}
	logger.Printf("model providers: %v", brain.ProvidersOf(keys))
	cfg, personas, err := config.LoadFor(*cfgPath, brain.ProvidersOf(keys))
	if err != nil {
		logger.Fatal(err)
	}
	l, err := ledger.Open(cfg.DataDir, cfg.DailyBudgetUSD)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Print(l.Summary())

	// guildnet.yaml and personas/ seed the org chart. With a database, they
	// only seed it once; after that the database is the source of truth.
	seed := org.SeedFrom(cfg, personas)
	var store org.Store = org.NewMemory(seed)
	if cfg.DatabaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		pg, err := org.OpenPostgres(ctx, cfg.DatabaseURL, seed)
		cancel()
		if err != nil {
			logger.Fatal(err)
		}
		defer pg.Close()
		store = pg
		logger.Print("org chart: postgres")
	} else {
		logger.Print("org chart: in memory (set GUILDNET_DATABASE_URL to keep changes across restarts)")
	}

	// GitHub, as the company's GitHub App: agents at a desk read the repos
	// read-only, and change them through tools the runner carries out.
	var toolbox agent.Toolbox
	// Agents in chat have no workspace: they read code through tools too.
	var chatTools agent.Toolbox
	var repos managed.Repos
	// Which repos a team works on is the org's to say (guildnet.yaml's
	// github.teams are carried into it once, at start).
	teamRepos := func(team string) []string { return org.ReposFor(context.Background(), store, team) }
	switch {
	case os.Getenv("GUILDNET_GITHUB_BROKER") != "":
		// A customer's network: tokens from the platform's broker, for the
		// installation its owner connected (or will connect: it's asked
		// again every couple of minutes).
		broker := github.Broker{URL: os.Getenv("GUILDNET_GITHUB_BROKER"), Network: os.Getenv("GUILDNET_NETWORK"), Secret: cfg.API.Token, HTTP: brokerHTTP}
		live := &github.Live{}
		check := &monitor.Check{Logger: logger}
		refresh := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := live.Refresh(ctx, broker); err != nil {
				check.Failed("github: asking the broker: %v", err)
				return
			}
			check.OK()
		}
		refresh()
		go func() {
			for range time.Tick(2 * time.Minute) {
				refresh()
			}
		}()
		cfg.GitHubLive = live.Get
		gh, err := github.New(github.Options{Tokens: broker, Live: live, TeamRepos: teamRepos})
		if err != nil {
			logger.Fatal(err)
		}
		toolbox, repos, chatTools = gh, gh, gh.WithoutWorkspace()
		account, list := live.Get()
		logger.Printf("github: through the platform's broker (%s: %v)", account, list)
	case cfg.GitHubEnabled():
		gh, err := github.New(github.Options{AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID,
			Key: cfg.GitHub.Key, Owner: cfg.GitHub.Owner, Repos: cfg.GitHub.Repos, TeamRepos: teamRepos})
		if err != nil {
			logger.Fatal(err)
		}
		toolbox, repos, chatTools = gh, gh, gh.WithoutWorkspace()
		logger.Printf("github: %s/%v", cfg.GitHub.Owner, cfg.GitHub.Repos)
	default:
		logger.Print("github: off (set the GUILDNET_GITHUB_* app settings to turn it on)")
	}

	// Connectors (Linear, Notion...), which the network's owner connects on
	// the platform: their tools join GitHub's, for every kind of agent. The
	// services' tools are listed now and every few minutes, with access
	// tokens from the platform's broker.
	var conn *connectors.Toolbox
	if url := connectorBroker(os.Getenv); url != "" {
		conn = connectors.NewToolbox(connectors.Options{
			Broker: connectors.Broker{URL: url, Network: os.Getenv("GUILDNET_NETWORK"), Secret: cfg.API.Token, HTTP: brokerHTTP},
			Store:  store, Teams: func(team string) []string { return org.ConnectorsFor(context.Background(), store, team) },
			Logger: logger,
			// MCP servers of networks' own on this install's private network,
			// where it names them.
			Egress: egress.Options{PrivateHosts: listFrom(os.Getenv("GUILDNET_CONNECTOR_PRIVATE_HOSTS"))},
			// Writes that ran without asking a person go in the org log.
			Wrote: func(p config.Persona, connector, tool, detail string) {
				e := org.LogEntry{At: time.Now(), Actor: p.Nick, Action: "used connector tool", Target: connector + " " + tool, Team: p.Team, Detail: detail}
				if err := store.AddLog(context.Background(), e); err != nil {
					monitor.Problem(logger, "org log: %v", err)
				}
			},
		})
		check := &monitor.Check{Logger: logger}
		refresh := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := conn.Refresh(ctx); err != nil {
				check.Failed("connectors: %v", err)
				return
			}
			check.OK()
		}
		go func() {
			refresh()
			for range time.Tick(3 * time.Minute) {
				refresh()
			}
		}()
		toolbox, chatTools = agent.Join(toolbox, conn), agent.Join(chatTools, conn)
	}
	// What agents read is kept track of even with GitHub's tools alone: its
	// private and public repos are sources of their own.
	chatTools = agent.Tracked(chatTools)

	// Agents run on their own platform: plain chat, or a desk with tools on
	// Managed Agents when GUILDNET_MANAGED=on (the beta's agent platform).
	var desks runner.AgentPlatform
	var sdk *managed.SDKDesks
	if os.Getenv("GUILDNET_MANAGED") == "on" && keys[models.Anthropic] == "" {
		logger.Print("no Anthropic key, so no desks (Managed Agents runs Claude): agents work in chat")
	} else if os.Getenv("GUILDNET_MANAGED") == "on" {
		budget, _ := strconv.ParseInt(os.Getenv("GUILDNET_DESK_BUDGET_CENTS"), 10, 64)
		network := os.Getenv("GUILDNET_NETWORK")
		if network == "" {
			network = "guildnet"
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var err error
		sdk, err = managed.NewSDKDesks(ctx, managed.SDKOptions{
			Network: network, BudgetCents: budget, Repos: repos, APIKey: keys[models.Anthropic],
			AllowedHosts: []string{"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
				// Go modules and toolchains, so agents can build and test.
				"proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "dl.google.com"},
		})
		cancel()
		if err != nil {
			// The key may not have Managed Agents: agents stay in chat rather
			// than the network going down.
			logger.Printf("Managed Agents isn't available (%v): agents work in chat only", err)
		} else {
			desks = managed.New(sdk, store, managed.Options{Company: cfg.Company, Owner: cfg.Owner, Toolbox: toolbox})
			logger.Print("agent platforms: chat, managed")
		}
	}

	chat := brain.New(brain.Options{Company: cfg.Company, Owner: cfg.Owner, Keys: keys, Toolbox: chatTools})
	// How agents outside GuildNet call models, if they can: on the same keys.
	var outside Models
	if build.Models != nil {
		outside = build.Models(keys)
	}
	if keyBroker != nil {
		// Keys can change while the network runs: fetched again every few
		// minutes, and whenever the platform says they changed.
		check := &monitor.Check{Logger: logger}
		refresh := func() {
			k, err := keyBroker.Keys(context.Background())
			if err != nil {
				check.Failed("model keys: %v", err)
				return
			}
			check.OK()
			chat.SetKeys(k)
			if outside != nil {
				outside.SetKeys(k)
			}
			if sdk != nil && k[models.Anthropic] != "" {
				sdk.UseKey(k[models.Anthropic])
			}
			if settings == nil {
				return
			}
			if s, err := fetchSettings(context.Background(), keyBroker); err == nil && s != *settings {
				logger.Print("the network's settings changed: restarting to take them up")
				select {
				case restart <- struct{}{}:
				default:
				}
			}
		}
		cfg.ProvidersLive = chat.Providers
		cfg.RefreshModelKeys = func() { go refresh() }
		go func() {
			for range time.Tick(2 * time.Minute) {
				refresh()
			}
		}()
	}
	router := runner.NewRouter(chat, desks)
	// Git repositories on any host, which the network's owner adds in the
	// console: listed now and every couple of minutes.
	var git *gitremote.Live
	if url := connectorBroker(os.Getenv); url != "" {
		git = &gitremote.Live{Broker: gitremote.Broker{URL: url, Network: os.Getenv("GUILDNET_NETWORK"), Secret: cfg.API.Token, HTTP: brokerHTTP}}
		check := &monitor.Check{Logger: logger}
		refresh := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := git.Refresh(ctx); err != nil {
				check.Failed("git repositories: asking the broker: %v", err)
				return
			}
			check.OK()
		}
		go func() {
			refresh()
			for range time.Tick(2 * time.Minute) {
				refresh()
			}
		}()
	}
	// Desks of GuildNet's own, when this network has a sandbox: the runner's
	// own loop, on any model, with a workspace in sandboxd. Its tokens are
	// the network's, signed by the platform and good for a few minutes: from
	// the platform's broker, which also wakes the sandbox VM when it's
	// asleep, or (on a developer's machine, given the platform's key) signed
	// here.
	sandboxURL := os.Getenv("GUILDNET_SANDBOX_URL")
	// The hosts the network's owner lets desks reach (the runner keeps the
	// list), and the desks' tool to ask for one.
	deskHosts := &runner.DeskHosts{}
	deskBlocked := listFrom(os.Getenv("GUILDNET_DESK_BLOCKED"))
	var tokens desk.TokenSource
	var pass func(context.Context) (string, error)
	if sandboxURL != "" {
		key, err := sandbox.PrivateKeyFrom(os.Getenv("GUILDNET_SANDBOX_KEY"))
		if err != nil {
			logger.Fatalf("GUILDNET_SANDBOX_URL needs GUILDNET_SANDBOX_KEY (from sandboxd keygen) to sign desk tokens: %v", err)
		}
		network := os.Getenv("GUILDNET_NETWORK")
		tokens = func(context.Context, bool) (string, error) {
			return sandbox.TokenFor(key, network, sandbox.Desks, time.Now(), 15*time.Minute), nil
		}
		pass = deskPass(key, network, deskHosts.Hosts, deskBlocked)
	}
	useSandbox := func(sandboxURL string, tokens desk.TokenSource, wakeSandbox func(context.Context) error, deskBroker *desk.Broker, pass func(context.Context) (string, error)) {
		sd := desk.NewRemoteFrom(sandboxURL, tokens)
		// Over TLS, sandboxd's certificate is from the CA the broker's is:
		// trusted the same way, and nothing else.
		if strings.HasPrefix(sandboxURL, "https://") && brokerHTTP.Transport != nil {
			sd.Through(brokerHTTP.Transport)
		}
		ws := desk.New(sd, repos)
		ws.Wake = wakeSandbox
		ws.Pass = pass
		// The runner fetches desks' repos for them, GitHub's and other hosts':
		// desks can't reach git hosts, and no git credential reaches one. A
		// first checkout of a big repository takes a while.
		ws.HTTP = egress.Client(egress.Options{PrivateHosts: gitPrivateHosts(os.Getenv)})
		ws.HTTP.Timeout = 30 * time.Minute
		// The team's git repositories on any host, pushed to by the runner
		// (the push credential never reaches a desk).
		var pusher agent.Toolbox
		if git != nil {
			ws.Git = gitremote.Checkouts{Live: git, TeamRepos: teamRepos}
			hosts := egress.Client(egress.Options{PrivateHosts: gitPrivateHosts(os.Getenv)})
			hosts.Timeout = 5 * time.Minute
			pusher = &gitremote.Pusher{Live: git, TeamRepos: teamRepos, Desk: sd, HTTP: hosts,
				Pushed: func(p config.Persona, repo, branch, commit string) {
					e := org.LogEntry{At: time.Now(), Actor: p.Nick, Action: "pushed a branch", Target: repo + " " + branch, Team: p.Team, Detail: commit}
					if err := store.AddLog(context.Background(), e); err != nil {
						monitor.Problem(logger, "org log: %v", err)
					}
				}}
		}
		router.WithDesks(chat.Desks(brain.DeskOptions{Toolbox: agent.Join(ws, toolbox, pusher, deskHosts), Ready: ws.Ready, Retire: ws.Clear}))
		// The platform counts desks' time against the network's plan (on a
		// developer's machine, with the sandbox from the environment, it
		// isn't counted).
		if deskBroker != nil {
			router.MeterDesks(runner.DeskMetering{Meter: deskBroker, Logger: logger})
		}
		logger.Printf("desks: in the sandbox at %s", sandboxURL)
	}
	// From the platform's broker: asked now, so a sandbox there at the start
	// is set up before agents wake, and every minute until there is one, so
	// desks turned on later are taken up without a restart.
	asking, stopAsking := context.WithCancel(context.Background())
	defer stopAsking()
	switch {
	case sandboxURL != "":
		useSandbox(sandboxURL, tokens, nil, nil, pass)
	case keyBroker != nil:
		b := &desk.Broker{URL: keyBroker.URL, Network: keyBroker.Network, Secret: keyBroker.Secret, HTTP: keyBroker.HTTP}
		check := &monitor.Check{Logger: logger}
		untilSandbox(asking, func(ctx context.Context) (string, error) {
			url, _, err := b.Sandbox(ctx)
			return url, err
		}, time.Minute, func(url string) {
			useSandbox(url, b.Tokens(), b.Wake, b, desk.KeptPass(b.Pass, deskHosts.Hosts, 4*time.Minute, nil))
		}, func(err error) { check.Failed("desks: %v", err) })
	}
	r := runner.New(cfg, store, router, l, logger)
	r.UseDeskHosts(deskHosts, deskBlocked...)
	if outside != nil {
		r.UseModels(outside.Handler)
	}
	if conn != nil {
		r.UseConnectors(conn)
	}
	if git != nil {
		r.UseGitRepos(git)
	}
	if chatTools != nil {
		// Agents outside GuildNet have no workspace either.
		r.UseTools(chatTools)
	}
	// News for the platform (approvals waiting, the spending cap reached),
	// which emails whoever wants it: through the same broker as the keys.
	if keyBroker != nil {
		r.NotifyWith(runner.BrokerNews{URL: keyBroker.URL, Network: keyBroker.Network, Secret: keyBroker.Secret, HTTP: brokerHTTP})
		// And how people sign in to the console: two-step accounts change
		// the org only from there, not over plain IRC.
		r.UseTwoSteps(runner.BrokerTwoSteps{URL: keyBroker.URL, Network: keyBroker.Network, Secret: keyBroker.Secret, HTTP: brokerHTTP})
	}
	if err := r.Start(); err != nil {
		logger.Fatal(err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
	case <-restart:
	}
	logger.Print("shutting down")
	r.Stop()
}

// untilSandbox asks for the sandbox's address now, then every `every` until
// there is one or ctx ends, and sets desks up there (use) once. Each ask has
// 30 seconds; failed hears why one failed.
func untilSandbox(ctx context.Context, ask func(context.Context) (string, error), every time.Duration, use func(url string), failed func(error)) {
	try := func() bool {
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		url, err := ask(actx)
		if err != nil {
			failed(err)
			return false
		}
		if url == "" {
			return false
		}
		use(url)
		return true
	}
	if try() {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if try() {
					return
				}
			}
		}
	}()
}

// brokerClient is how the runner reaches the platform's broker: over TLS,
// trusting only GuildNet's own CA, when it's given the CA's certificate
// (public), by the host in its settings (GUILDNET_BROKER_CA) or, on a
// self-hosted install, as a file the console shares (GUILDNET_BROKER_CA_FILE,
// read at each connection).
func brokerClient(getenv func(string) string) (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if b64 := getenv("GUILDNET_BROKER_CA"); b64 != "" {
		caPEM, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("GUILDNET_BROKER_CA: %v", err)
		}
		tlsConfig, err := pki.ClientTLS(caPEM)
		if err != nil {
			return nil, fmt.Errorf("GUILDNET_BROKER_CA: %v", err)
		}
		client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	} else if file := getenv("GUILDNET_BROKER_CA_FILE"); file != "" {
		// Checked against the host dialled: the broker by name, sandboxd on a
		// desks host of its own by name or address.
		client.Transport = pki.ClientTransport(file)
	}
	if client.Transport != nil {
		// Given a CA, TLS or nothing: a broker named with http:// never sees
		// the network's secret.
		client.Transport = tlsOnly{client.Transport}
	}
	return client, nil
}

// tlsOnly refuses any request that isn't https.
type tlsOnly struct{ next http.RoundTripper }

func (t tlsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("%s isn't https: with GuildNet's CA set, the broker is only reached over TLS", req.URL.Redacted())
	}
	return t.next.RoundTrip(req)
}

// gitPrivateHosts are git servers on the install's own network, named by
// whoever runs it; everywhere else, git hosts must be on the internet.
func gitPrivateHosts(getenv func(string) string) []string {
	return listFrom(getenv("GUILDNET_GIT_PRIVATE_HOSTS"))
}

// deskPass is the network's egress pass, signed here as the platform would:
// the hosts its owner lets desks reach (hosts), on 443, less any blocked.
// Not its git hosts: the runner fetches and pushes for desks, which reach
// no git host at all. "" when there are none.
func deskPass(key ed25519.PrivateKey, network string, hosts func() []string, blocked []string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		var open []string
		for _, h := range hosts() {
			if h, problem := sandbox.CheckDeskHost(h, blocked); problem == "" {
				open = append(open, h+":443")
			}
		}
		if len(open) == 0 {
			return "", nil
		}
		return sandbox.PassFor(key, network, open, time.Now(), 15*time.Minute), nil
	}
}

// listFrom is a comma-separated setting's items.
func listFrom(v string) []string {
	var out []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// connectorBroker is where the runner asks for its connectors' tokens: the
// platform's broker, as named for its model keys or, when the network keeps
// its own keys, for GitHub. "" means there's none.
func connectorBroker(getenv func(string) string) string {
	if url := getenv("GUILDNET_KEYS_BROKER"); url != "" {
		return url
	}
	return getenv("GUILDNET_GITHUB_BROKER")
}
