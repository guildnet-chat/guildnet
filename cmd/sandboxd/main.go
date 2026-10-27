// SPDX-License-Identifier: AGPL-3.0-only

// Command sandboxd keeps the desks of GuildNet's own, on the sandbox VM (or
// this machine, for development):
//
//	sandboxd serve    the API runners reach their desks through
//	sandboxd proxy    the desks' only way out: HTTPS to allowed hosts
//	sandboxd keygen   a new key pair, as settings: the platform's private
//	                  GUILDNET_SANDBOX_KEY, sandboxd's GUILDNET_SANDBOX_PUBLIC_KEY
//	sandboxd tls-request   on a desks host of its own: a request for serve's
//	                  certificate, for the console to sign (platform
//	                  sandbox-cert); the key stays in GUILDNET_SANDBOX_TLS_DIR
//	sandboxd tls-install   the certificate the console signed, from stdin,
//	                  into GUILDNET_SANDBOX_TLS_DIR, where serve picks it up
//
// Runners' tokens are signed by the platform (or, for development, by a
// runner given GUILDNET_SANDBOX_KEY itself) and good for a few minutes.
//
// serve needs the Docker socket and the public key; proxy needs neither,
// and sits on the desks' network and one with a route out.
package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/pki"
	"github.com/guildnet-chat/guildnet/serve"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := log.New(os.Stderr, "sandboxd: ", log.LstdFlags)
	// A failure ends it with 1, once what's deferred (the fault reports'
	// flush) has run.
	failed := false
	defer func() {
		if failed {
			os.Exit(1)
		}
	}()
	// Faults in GuildNet itself go to GuildNet's Sentry, when it's set.
	defer monitor.Start(monitor.FromEnv("sandboxd"))()
	defer monitor.Recover()
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serveDesks(logger)
	case "proxy":
		err = proxy(logger)
	case "keygen":
		public, private := sandbox.NewKeys()
		fmt.Printf("GUILDNET_SANDBOX_KEY=%s\nGUILDNET_SANDBOX_PUBLIC_KEY=%s\n", sandbox.KeyText(private), sandbox.KeyText(public))
	case "tls-request", "tls-install":
		dir := os.Getenv("GUILDNET_SANDBOX_TLS_DIR")
		if dir == "" {
			err = fmt.Errorf("%s needs GUILDNET_SANDBOX_TLS_DIR, where serve's key and certificate are", cmd)
			break
		}
		if cmd == "tls-request" {
			var csr []byte
			if csr, err = pki.Request(dir, "sandboxd"); err == nil {
				_, err = os.Stdout.Write(csr)
			}
			break
		}
		var cert []byte
		if cert, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10)); err == nil {
			err = pki.Install(dir, "sandboxd", cert)
		}
	default:
		err = fmt.Errorf("unknown command %q: serve, proxy, keygen, tls-request or tls-install", cmd)
	}
	if err != nil {
		logger.Print(err)
		failed = true
	}
}

// filesFull is how full, in percent, the desks' disk, or Docker's, may get
// before it's reported.
const filesFull = 80

// watchDisks reports the desks' disk, and the one Docker keeps images and
// containers on, if either is past filesFull.
func watchDisks(logger *log.Logger, backend *sandbox.Docker) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if backend.Files != "" {
		switch pct, err := backend.DiskUse(ctx); {
		case err != nil:
			logger.Printf("how full the desks' disk is: %v", err)
		case pct >= filesFull:
			monitor.Problem(logger, "the desks' disk is %d%% full: give it more room (docs/self-host/desks.md#storage)", pct)
		}
	}
	switch pct, where, err := backend.DockerDiskUse(ctx); {
	case err != nil:
		logger.Printf("how full Docker's disk is: %v", err)
	case pct >= filesFull:
		monitor.Problem(logger, "the disk Docker keeps images and containers on (%s) is %d%% full: give the machine more disk, or remove images it no longer needs (docker image prune)", where, pct)
	}
}

func serveDesks(logger *log.Logger) error {
	// The platform's key, or keys while it's changed (comma-separated).
	var keys []ed25519.PublicKey
	for _, text := range strings.Split(os.Getenv("GUILDNET_SANDBOX_PUBLIC_KEY"), ",") {
		key, err := sandbox.PublicKeyFrom(text)
		if err != nil {
			return fmt.Errorf("GUILDNET_SANDBOX_PUBLIC_KEY (from sandboxd keygen): %w", err)
		}
		keys = append(keys, key)
	}
	pids, _ := strconv.Atoi(env("GUILDNET_DESK_PIDS", "512"))
	backend := &sandbox.Docker{
		Image:   env("GUILDNET_DESK_IMAGE", "guildnet-desk:latest"),
		Runtime: os.Getenv("GUILDNET_DESK_RUNTIME"), // runsc on the sandbox VM
		Network: env("GUILDNET_DESK_NETWORK", "guildnet-desks"),
		Proxy:   env("GUILDNET_DESK_PROXY", "http://egress:3128"),
		Memory:  env("GUILDNET_DESK_MEMORY", "3g"),
		CPUs:    env("GUILDNET_DESK_CPUS", "2"),
		Pids:    pids,
		// Desks' files in a folder on the host, one per network, rather than
		// volumes: on XFS mounted with prjquota, each network's is limited.
		Files: os.Getenv("GUILDNET_DESK_FILES"),
	}
	// A network's desks' files' limit when its token doesn't say (a console
	// of one's own): GB, or none.
	defaultGB, _ := strconv.Atoi(os.Getenv("GUILDNET_DESK_STORAGE_GB"))
	// How many desks run at once: as many as fit the VM's memory at each
	// desk's limit, unless set.
	atOnce, _ := strconv.Atoi(os.Getenv("GUILDNET_DESKS_AT_ONCE"))
	if atOnce <= 0 {
		size, err := sandbox.ParseMemory(backend.Memory)
		if err != nil {
			return fmt.Errorf("GUILDNET_DESK_MEMORY: %w", err)
		}
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return err
		}
		total, err := sandbox.MemTotal(f)
		f.Close()
		if err != nil {
			return err
		}
		atOnce = sandbox.DesksFor(total, size)
	}
	// Desks left running from before know nothing of this sandboxd's count.
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := backend.StopAll(stopCtx); err != nil {
		logger.Printf("stopping desks left running: %v", err)
	}
	cancel()
	// Desks are kept apart by the desks' firewall, which is checked, not
	// taken on trust: if one desk reaches another, sandboxd won't serve.
	// Only an install trying desks out without it says so (off).
	if os.Getenv("GUILDNET_DESK_FIREWALL") != "off" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		apart, err := backend.Apart(ctx)
		cancel()
		switch {
		case err != nil:
			monitor.Problem(logger, "checking desks are kept apart: %v", err)
		case !apart:
			return fmt.Errorf("a desk on %s reached another: the desks' firewall isn't keeping them apart (desks-firewall.sh); GUILDNET_DESK_FIREWALL=off serves without it, only to try desks out", backend.Network)
		default:
			logger.Printf("desks are kept apart (checked)")
		}
	}
	// Shared by several networks (guildnet.chat's sandbox VM), each has only
	// so many of them, unless unset.
	perNetwork, _ := strconv.Atoi(os.Getenv("GUILDNET_DESKS_PER_NETWORK"))
	s := sandbox.NewServer(sandbox.Options{Backend: backend, PublicKeys: keys, Logger: logger, MaxRunning: atOnce, MaxPerNetwork: perNetwork, DefaultStorageGB: defaultGB})
	logger.Printf("room for %d desks at once, %s each", atOnce, backend.Memory)
	// Until it's told to stop (SIGTERM): then calls in flight finish.
	ctx, stop := serve.Signalled()
	defer stop()
	go serve.Every(ctx, time.Minute, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		s.StopIdle(ctx)
		cancel()
	})
	if backend.Files != "" {
		logger.Printf("desks' files in %s", backend.Files)
	}
	// Networks' limits can add up to more than the desks' disk, and desks'
	// images (rebuilt at each deploy) fill Docker's: both are watched.
	go serve.Every(ctx, 10*time.Minute, func(context.Context) { watchDisks(logger, backend) })
	listen := env("GUILDNET_SANDBOX_LISTEN", ":8443")
	runtime := backend.Runtime
	if runtime == "" {
		runtime = "Docker's own runtime"
	}
	logger.Printf("serving desks on %s: image %s, %s, network %s", listen, backend.Image, runtime, backend.Network)
	srv := &http.Server{Addr: listen, Handler: monitor.Handler(s), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	// Over TLS, with the certificate the console gives it from its own CA
	// (sandboxd.crt and .key there, picked up again when renewed).
	if dir := os.Getenv("GUILDNET_SANDBOX_TLS_DIR"); dir != "" {
		srv.TLSConfig = pki.ServerTLS(filepath.Join(dir, "sandboxd.crt"), filepath.Join(dir, "sandboxd.key"))
		logger.Printf("over TLS, with the certificate in %s", dir)
	}
	return serve.Until(ctx, shutdownGrace, serve.HTTP{Server: srv})
}

// shutdownGrace is how long calls in flight get to finish once sandboxd is
// told to stop: within Docker's 10 seconds before it kills it.
const shutdownGrace = 8 * time.Second

func proxy(logger *log.Logger) error {
	allowed := sandbox.DefaultAllowed
	if v := os.Getenv("GUILDNET_DESK_ALLOWED"); v != "" {
		allowed = strings.Split(v, ",")
	}
	// Networks' egress passes, signed by the platform, open the hosts their
	// owners chose. Never a private address: desks reach no git host (the
	// runner fetches for them), so not even git servers on the install's own
	// network.
	var keys []ed25519.PublicKey
	for _, text := range strings.Split(os.Getenv("GUILDNET_SANDBOX_PUBLIC_KEY"), ",") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		key, err := sandbox.PublicKeyFrom(text)
		if err != nil {
			return fmt.Errorf("GUILDNET_SANDBOX_PUBLIC_KEY (from sandboxd keygen): %w", err)
		}
		keys = append(keys, key)
	}
	p := &sandbox.Proxy{Allowed: allowed, Keys: keys, Dial: egress.Dial(egress.Options{}), Logger: logger, Reads: sandbox.DefaultReads}
	listen := env("GUILDNET_PROXY_LISTEN", ":3128")
	logger.Printf("desks' way out on %s, to %s, and the hosts networks' passes open (%d keys)", listen, strings.Join(allowed, ", "), len(keys))
	srv := &http.Server{Addr: listen, Handler: monitor.Handler(p), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	ctx, stop := serve.Signalled()
	defer stop()
	return serve.Until(ctx, shutdownGrace, serve.HTTP{Server: srv})
}
