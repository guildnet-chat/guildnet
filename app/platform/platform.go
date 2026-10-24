// SPDX-License-Identifier: AGPL-3.0-only

// Package app runs GuildNet's console from its settings (the environment):
// what the open build's command is, and what a build with more plugged in
// (GuildNet's hosted edition) starts from.
package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/netspec"
	"github.com/guildnet-chat/guildnet/pki"
	"github.com/guildnet-chat/guildnet/platform"
	"github.com/guildnet-chat/guildnet/serve"
)

// Store is what the console keeps its own data in.
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
	DeleteExpired(ctx context.Context, now time.Time) error
}

// Config is what a build plugs into the console; its zero value is the
// console on its own.
type Config struct {
	// Host makes the machine's host agent, which runs new networks, from
	// the settings; nil (or it returning nil) means networks aren't run
	// here, only the one the settings describe (GUILDNET_LOCAL_NETWORK).
	Host func(logger *log.Logger) platform.Host
	// Extension makes what's plugged in (platform.Extension), given the
	// console's options and data; nil means nothing is.
	Extension func(opts platform.Options, s Store, logger *log.Logger) platform.Extension
	// ChatCommit is the chat app's commit (the build sets it), for its
	// Source link.
	ChatCommit string
}

// Run serves the console, as its settings say, until it can't.
func Run(cfg Config) {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	// Serving that fails ends the program with 1, once what's deferred has
	// run.
	failed := false
	defer func() {
		if failed {
			os.Exit(1)
		}
	}()
	// healthz: whether this copy is serving, for Docker's health check (the
	// image has no shell or curl), every few seconds; nothing is served, and
	// nothing else is started.
	if len(os.Args) > 1 && os.Args[1] == "healthz" {
		listen := os.Getenv("GUILDNET_PLATFORM_LISTEN")
		if listen == "" {
			listen = ":8080"
		}
		_, port, _ := net.SplitHostPort(listen)
		c := &http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			failed = true
			return
		}
		resp.Body.Close()
		failed = resp.StatusCode != http.StatusOK
		return
	}
	// Faults in GuildNet itself go to GuildNet's Sentry, when it's set.
	defer monitor.Start(monitor.FromEnv("platform"))()
	defer monitor.Recover()
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	listen := env("GUILDNET_PLATFORM_LISTEN", ":8080")
	base := env("GUILDNET_BASE_DOMAIN", "guildnet.localhost")

	// Once told to stop, how long it goes on serving (saying it's stopping,
	// at /healthz) for whatever's in front to move off it, and how long
	// requests in flight, then what it began in the background, get to
	// finish. Within Docker's time before it kills it (10 seconds unless the
	// stack says).
	drain, err := time.ParseDuration(env("GUILDNET_STOP_DRAIN", "0s"))
	if err != nil {
		logger.Fatalf("GUILDNET_STOP_DRAIN: %v", err)
	}
	grace, err := time.ParseDuration(env("GUILDNET_STOP_GRACE", "4s"))
	if err != nil {
		logger.Fatalf("GUILDNET_STOP_GRACE: %v", err)
	}

	// sandbox-cert: sandboxd's certificate for a desks host of its own, from
	// its request on stdin (sandboxd tls-request there), signed with the
	// console's CA for GUILDNET_SANDBOX_NAMES; nothing is served.
	if len(os.Args) > 1 && os.Args[1] == "sandbox-cert" {
		dir := os.Getenv("GUILDNET_BROKER_TLS_DIR")
		if dir == "" {
			logger.Fatal("sandbox-cert signs with the console's CA: set GUILDNET_BROKER_TLS_DIR")
		}
		names := strings.Split(env("GUILDNET_SANDBOX_NAMES", "sandboxd"), ",")
		if err := signSandboxCert(dir, names, os.Stdin, os.Stdout, time.Now()); err != nil {
			logger.Fatalf("sandbox-cert: %v", err)
		}
		return
	}

	var s Store = platform.NewMemoryStore()
	if url := os.Getenv("GUILDNET_PLATFORM_DATABASE_URL"); url != "" {
		key, err := base64.StdEncoding.DecodeString(os.Getenv("GUILDNET_PLATFORM_KEY"))
		if err != nil {
			logger.Fatalf("GUILDNET_PLATFORM_KEY must be 32 random bytes, base64-encoded: %v", err)
		}
		// While the key's changed: the old ones, which still open what they
		// sealed (each is sealed again with the new key when it's next saved).
		var old [][]byte
		for _, text := range strings.FieldsFunc(os.Getenv("GUILDNET_PLATFORM_OLD_KEYS"), func(r rune) bool { return r == ',' }) {
			k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
			if err != nil {
				logger.Fatalf("GUILDNET_PLATFORM_OLD_KEYS: %v", err)
			}
			old = append(old, k)
		}
		sealer, err := platform.NewAESSealer(key, old...)
		if err != nil {
			logger.Fatalf("GUILDNET_PLATFORM_KEY: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		pg, err := platform.OpenPostgres(ctx, url, sealer)
		cancel()
		if err != nil {
			logger.Fatal(err)
		}
		defer pg.Close()
		s = pg
		logger.Print("platform data: postgres")
	} else {
		logger.Print("platform data: in memory (set GUILDNET_PLATFORM_DATABASE_URL to keep sign-ins across restarts)")
	}

	// Until networks are created through the platform (M2 step 4), the
	// local network comes from the environment.
	if name := os.Getenv("GUILDNET_LOCAL_NETWORK"); name != "" {
		n := platform.Network{
			Name: name, Region: "local",
			RunnerURL: os.Getenv("GUILDNET_LOCAL_RUNNER_URL"), RunnerToken: os.Getenv("GUILDNET_API_TOKEN"),
			ErgoURL: os.Getenv("GUILDNET_LOCAL_ERGO_URL"), ErgoToken: os.Getenv("GUILDNET_ERGO_API_TOKEN"),
			// Web chat, if its Ergo has a WebSocket listener and trusts the key.
			ChatURL: os.Getenv("GUILDNET_LOCAL_CHAT_URL"), ChatKey: os.Getenv("GUILDNET_CHAT_KEY"),
		}
		if n.RunnerURL == "" || n.RunnerToken == "" || n.ErgoURL == "" || n.ErgoToken == "" {
			logger.Fatal("the local network needs GUILDNET_LOCAL_RUNNER_URL, GUILDNET_API_TOKEN, GUILDNET_LOCAL_ERGO_URL, and GUILDNET_ERGO_API_TOKEN")
		}
		if err := s.PutNetwork(context.Background(), n); err != nil {
			logger.Fatal(err)
		}
		logger.Printf("network %s: http://%s.%s%s", name, name, base, listen)
	}

	// A self-hosted install: its one network (GUILDNET_NETWORK), which
	// first-run setup makes. Its IRC server and runner start with the
	// install, and the runner gets its settings and keys from the broker.
	var firstRun *platform.FirstRun
	if name := os.Getenv("GUILDNET_NETWORK"); name != "" && cfg.Host == nil {
		if !netspec.ValidName(name) {
			logger.Fatal("GUILDNET_NETWORK is 2 to 30 lowercase letters, digits and single hyphens, starting with a letter")
		}
		if os.Getenv("GUILDNET_LOCAL_NETWORK") != "" {
			logger.Fatal("set GUILDNET_NETWORK (a network first-run setup makes) or GUILDNET_LOCAL_NETWORK (one set up by hand), not both")
		}
		firstRun = &platform.FirstRun{Network: name,
			Addresses:   netspec.Addresses{RunnerURL: os.Getenv("GUILDNET_RUNNER_URL"), ErgoURL: os.Getenv("GUILDNET_ERGO_URL"), ChatURL: os.Getenv("GUILDNET_CHAT_URL")},
			RunnerToken: os.Getenv("GUILDNET_API_TOKEN"), ErgoToken: os.Getenv("GUILDNET_ERGO_API_TOKEN"), ChatKey: os.Getenv("GUILDNET_CHAT_KEY")}
		switch {
		case firstRun.Addresses.RunnerURL == "" || firstRun.Addresses.ErgoURL == "" || firstRun.RunnerToken == "" || firstRun.ErgoToken == "":
			logger.Fatal("the network needs GUILDNET_RUNNER_URL, GUILDNET_ERGO_URL, GUILDNET_API_TOKEN and GUILDNET_ERGO_API_TOKEN")
		case os.Getenv("GUILDNET_GITHUB_BROKER_LISTEN") == "":
			logger.Fatal("the network's runner gets its settings and keys from the broker: set GUILDNET_GITHUB_BROKER_LISTEN")
		case os.Getenv("GUILDNET_PLATFORM_DATABASE_URL") == "":
			logger.Fatal("first-run setup needs GUILDNET_PLATFORM_DATABASE_URL: in memory, the network would be forgotten when the console restarts")
		}
	}

	// reset-owner: a link for the network's owner, locked out, to set a new
	// password with, for whoever runs the server; nothing is served.
	if len(os.Args) > 1 && os.Args[1] == "reset-owner" {
		if firstRun == nil {
			logger.Fatal("reset-owner is for a self-hosted install's network (GUILDNET_NETWORK)")
		}
		link, err := platform.ResetOwner(context.Background(), platform.Options{BaseDomain: base, Networks: s, Links: s,
			SecureCookies: strings.EqualFold(os.Getenv("GUILDNET_SECURE_COOKIES"), "on"), FirstRun: firstRun})
		if err != nil {
			logger.Fatal(err)
		}
		fmt.Printf("\nOpen this within the hour to set a new password for %s's owner. It works once, and turns their\ntwo-step sign-in off until they set it up again:\n\n%s\n\n", firstRun.Network, link)
		return
	}

	// Until it's told to stop (SIGTERM, at a deploy): then what's in flight
	// finishes, and Run returns.
	ctx, stop := serve.Signalled()
	defer stop()
	var servers []serve.Server

	// Expired sessions are cleared out hourly.
	go serve.Every(ctx, time.Hour, func(ctx context.Context) {
		if err := s.DeleteExpired(ctx, time.Now()); err != nil {
			monitor.Problem(logger, "clearing expired sessions: %v", err)
		}
	})

	// The edge in front (Caddy), whose X-Forwarded-For is believed.
	var proxies []netip.Prefix
	for _, cidr := range strings.Split(os.Getenv("GUILDNET_TRUSTED_PROXIES"), ",") {
		if cidr = strings.TrimSpace(cidr); cidr == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			logger.Fatalf("GUILDNET_TRUSTED_PROXIES: %v", err)
		}
		proxies = append(proxies, prefix)
	}

	opts := platform.Options{
		BaseDomain:     base,
		Networks:       s,
		Sessions:       s,
		People:         s,
		Links:          s,
		Onboarding:     s,
		Alerts:         s,
		Connections:    s,
		GitRepos:       s,
		SecureCookies:  strings.EqualFold(os.Getenv("GUILDNET_SECURE_COOKIES"), "on"),
		TrustedProxies: proxies,
		ChatDir:        os.Getenv("GUILDNET_CHAT_DIR"),
		ChatSource:     os.Getenv("GUILDNET_CHAT_SOURCE"),
		Source:         os.Getenv("GUILDNET_SOURCE"),
		ChatCommit:     cfg.ChatCommit,
		DevSignIn:      strings.EqualFold(os.Getenv("GUILDNET_DEV_SIGNIN"), "on"),
		FirstRun:       firstRun,
		Region:         env("GUILDNET_REGION", "local"),
		Logger:         logger,
	}
	// Google sign-in on the home page, with GuildNet's OAuth client.
	if id := os.Getenv("GUILDNET_GOOGLE_CLIENT_ID"); id != "" {
		opts.Google = &platform.GoogleSignIn{ClientID: id, ClientSecret: os.Getenv("GUILDNET_GOOGLE_CLIENT_SECRET")}
	}
	// Services that take client ID metadata documents know GuildNet by its
	// own, which must be on the internet, over HTTPS; elsewhere GuildNet
	// registers with each service.
	if opts.SecureCookies {
		opts.ClientMetadataURL = "https://" + base + "/oauth/client-metadata.json"
	}
	// On the internet, the owner and anyone who can change things across a
	// whole org sign in to its console in two steps.
	opts.TwoSteps = s
	opts.RequireTwoStep = opts.SecureCookies
	opts.HomeTwoSteps = s
	opts.RequireHomeTwoStep = opts.SecureCookies
	if opts.DevSignIn && opts.SecureCookies {
		logger.Fatal("GUILDNET_DEV_SIGNIN signs anyone in by email alone: it's for a developer's own machine, never with GUILDNET_SECURE_COOKIES")
	}
	// Git servers on the install's own network, which agents' repositories
	// may be on; everywhere else, git hosts must be on the internet.
	for _, h := range strings.Split(os.Getenv("GUILDNET_GIT_PRIVATE_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			opts.GitPrivateHosts = append(opts.GitPrivateHosts, h)
		}
	}
	// MCP servers on this install's own network that networks may add as
	// connectors of their own; everywhere else, they must be on the internet.
	for _, h := range strings.Split(os.Getenv("GUILDNET_CONNECTOR_PRIVATE_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			opts.ConnectorEgress.PrivateHosts = append(opts.ConnectorEgress.PrivateHosts, h)
		}
	}
	// Hosts no network's desks reach, whatever its owner's list says, beside
	// GuildNet's own.
	for _, h := range strings.Split(os.Getenv("GUILDNET_DESK_BLOCKED"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			opts.DeskBlocked = append(opts.DeskBlocked, h)
		}
	}
	// What this GuildNet goes by, and how it looks.
	brand, err := brandFrom(os.Getenv)
	if err != nil {
		logger.Fatal(err)
	}
	opts.Brand = brand
	// GuildNet's own email: through Resend, with a key that may only send
	// from GuildNet's sending domain, or through any mail server (SMTP).
	opts.Mail = mailer(logger)
	// GuildNet's public GitHub App, for customers' networks. Its key and
	// client secret stay here: runners get tokens from the broker.
	if id := os.Getenv("GUILDNET_GITHUB_PUBLIC_APP_ID"); id != "" {
		appID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			logger.Fatalf("GUILDNET_GITHUB_PUBLIC_APP_ID: %v", err)
		}
		key := []byte(os.Getenv("GUILDNET_GITHUB_PUBLIC_APP_KEY"))
		if !bytes.Contains(key, []byte("BEGIN")) {
			if key, err = base64.StdEncoding.DecodeString(string(key)); err != nil {
				logger.Fatal("GUILDNET_GITHUB_PUBLIC_APP_KEY must be the app's private key (PEM), or it base64-encoded onto one line")
			}
		}
		opts.GitHub = &platform.GitHubApp{AppID: appID, Slug: os.Getenv("GUILDNET_GITHUB_PUBLIC_APP_SLUG"), Key: key,
			ClientID: os.Getenv("GUILDNET_GITHUB_PUBLIC_CLIENT_ID"), ClientSecret: os.Getenv("GUILDNET_GITHUB_PUBLIC_CLIENT_SECRET")}
		if opts.GitHub.Slug == "" || opts.GitHub.ClientID == "" || opts.GitHub.ClientSecret == "" {
			logger.Fatal("the public GitHub App needs GUILDNET_GITHUB_PUBLIC_APP_SLUG, _CLIENT_ID and _CLIENT_SECRET too")
		}
		logger.Printf("github: the public app %s", opts.GitHub.Slug)
	}
	// The host agent that runs new networks on this machine.
	if cfg.Host != nil {
		if h := cfg.Host(logger); h != nil {
			opts.Host = h
			// Runners fetch their model keys from this platform's broker, if
			// it runs one: then the host is never sent them.
			opts.KeysByBroker = os.Getenv("GUILDNET_GITHUB_BROKER_LISTEN") != ""
		}
	}
	// Networks' IRC servers ask the broker about people's logins.
	opts.ErgoAsks = os.Getenv("GUILDNET_GITHUB_BROKER_LISTEN") != ""
	// IRC apps connect at the edge with TLS, on this port.
	if port := os.Getenv("GUILDNET_IRC_PORT"); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			logger.Fatalf("GUILDNET_IRC_PORT: %v", err)
		}
		opts.IRCPort = n
	}
	// Desks of GuildNet's own: sandboxd on the sandbox VM, which powers off
	// when idle. Waking it is the VM host's job (it holds the credentials
	// to start it; containers can't reach them): the platform writes the
	// time to a file the host watches.
	if url := os.Getenv("GUILDNET_SANDBOX_URL"); url != "" {
		key, err := sandbox.PrivateKeyFrom(os.Getenv("GUILDNET_SANDBOX_KEY"))
		if err != nil {
			logger.Fatalf("GUILDNET_SANDBOX_KEY (from sandboxd keygen) must be set with GUILDNET_SANDBOX_URL: %v", err)
		}
		opts.Sandbox = platform.Sandbox{URL: url, Key: key}
		if file := os.Getenv("GUILDNET_SANDBOX_WAKE_FILE"); file != "" {
			opts.Sandbox.Wake = func() error {
				return os.WriteFile(file, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
			}
		}
		// sandboxd over TLS, with a certificate from the console's own CA
		// (GUILDNET_BROKER_TLS_DIR's) or, hosted, from GuildNet's CA, which
		// the host holds and shares the certificate of: reached trusting that
		// CA alone, as the runner does. Beside a self-hosted console, sandboxd
		// is given one in GUILDNET_SANDBOX_TLS_DIR, a folder only it shares;
		// on a desks host of its own, it asks for one (sandbox-cert; hosted,
		// the host's).
		own := os.Getenv("GUILDNET_BROKER_TLS_DIR")
		if strings.HasPrefix(url, "https://") {
			ca, err := sandboxCA(os.Getenv)
			if err != nil {
				logger.Fatal(err)
			}
			opts.Sandbox.Through = pki.ClientTransport(ca)
		}
		if dir := os.Getenv("GUILDNET_SANDBOX_TLS_DIR"); dir != "" {
			if own == "" {
				logger.Fatal("GUILDNET_SANDBOX_TLS_DIR needs GUILDNET_BROKER_TLS_DIR: the console's CA, which sandboxd's certificate is from")
			}
			names := strings.Split(env("GUILDNET_SANDBOX_NAMES", "sandboxd"), ",")
			if err := sandboxCerts(own, dir, names, time.Now()); err != nil {
				logger.Fatalf("sandboxd's certificate: %v", err)
			}
			go serve.Every(ctx, 24*time.Hour, func(context.Context) {
				if err := sandboxCerts(own, dir, names, time.Now()); err != nil {
					monitor.Problem(logger, "renewing sandboxd's certificate: %v", err)
				}
			})
		}
		logger.Printf("desks: sandbox at %s", url)
	}
	if cfg.Extension != nil {
		opts.Extension = cfg.Extension(opts, s, logger)
	}
	p := platform.New(opts)
	// What the platform does every so often: finishing off deleted networks'
	// desks (once the sandbox is awake), and whatever's plugged in does.
	for _, job := range p.Jobs() {
		go serve.Every(ctx, job.Every, job.Run)
	}
	// The edge sends IRC apps that forgot TLS here, to be told so.
	if listen := os.Getenv("GUILDNET_IRC_HINT_LISTEN"); listen != "" {
		l, err := net.Listen("tcp", listen)
		if err != nil {
			logger.Fatal(err)
		}
		servers = append(servers, serve.Func(func() error {
			if err := p.ServeIRCHint(l); !errors.Is(err, net.ErrClosed) {
				return err
			}
			return nil
		}, func(context.Context) error { return l.Close() }))
	}
	// The edge asks here before getting a network's HTTPS certificate: a
	// listener of its own, never proxied to the internet.
	if listen := os.Getenv("GUILDNET_CERT_CHECK_LISTEN"); listen != "" {
		servers = append(servers, serve.HTTP{Server: &http.Server{Addr: listen, Handler: monitor.Handler(p.CertificateCheck()), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}})
	}
	// Networks' runners get their model keys and GitHub tokens here: a
	// listener of its own, on the networks' private Docker networks, never
	// proxied to the internet. Over TLS, with the certificate the host issues
	// it from GuildNet's own CA, when there is one.
	// A self-hosted install's broker, over TLS from a CA the console keeps
	// itself (GUILDNET_BROKER_TLS_DIR, a volume only it has), whose
	// certificate it shares with the runner (GUILDNET_BROKER_CA_SHARE).
	brokerTLS := os.Getenv("GUILDNET_BROKER_TLS_DIR")
	if brokerTLS != "" {
		share := os.Getenv("GUILDNET_BROKER_CA_SHARE")
		if share == "" {
			logger.Fatal("GUILDNET_BROKER_TLS_DIR needs GUILDNET_BROKER_CA_SHARE: where the runner reads the CA's certificate")
		}
		names := strings.Split(env("GUILDNET_BROKER_NAMES", "platform"), ",")
		if err := brokerCerts(brokerTLS, share, names, time.Now()); err != nil {
			logger.Fatalf("the broker's certificate: %v", err)
		}
		// Renewed well before it runs out.
		go serve.Every(ctx, 24*time.Hour, func(context.Context) {
			if err := brokerCerts(brokerTLS, share, names, time.Now()); err != nil {
				monitor.Problem(logger, "renewing the broker's certificate: %v", err)
			}
		})
		logger.Printf("broker: TLS for %s, from the console's own CA", strings.Join(names, ", "))
	}
	if listen := os.Getenv("GUILDNET_GITHUB_BROKER_LISTEN"); listen != "" {
		srv := &http.Server{Addr: listen, Handler: monitor.Handler(p.Broker()), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		if shared := os.Getenv("GUILDNET_PKI_SHARED"); shared != "" {
			srv.TLSConfig = pki.ServerTLS(filepath.Join(shared, "broker.crt"), filepath.Join(shared, "broker.key"))
		} else if brokerTLS != "" {
			srv.TLSConfig = pki.ServerTLS(filepath.Join(brokerTLS, "broker.crt"), filepath.Join(brokerTLS, "broker.key"))
		}
		servers = append(servers, serve.HTTP{Server: srv})
	}
	// /healthz, on any host: whether this copy is serving (Caddy checks it,
	// and moves to another copy when it says it's stopping).
	health := &serve.Health{}
	console := monitor.Handler(p.Handler())
	srv := &http.Server{Addr: listen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			health.ServeHTTP(w, r)
			return
		}
		console.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	// Listening before saying so: what waits for this line can connect.
	l, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Printf("platform listening on %s (landing page: http://%s%s)", listen, base, listen)
	servers = append(servers, serve.HTTP{Server: srv, Listener: l})
	if err := serve.Until(serve.Draining(ctx, health, drain, servers...), grace, servers...); err != nil {
		monitor.Problem(logger, "serving: %v", err)
		failed = true
		return
	}
	finishing, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := p.Finish(finishing); err != nil {
		monitor.Problem(logger, "%v", err)
	}
	logger.Print("stopped")
}
