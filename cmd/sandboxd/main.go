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
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := log.New(os.Stderr, "sandboxd: ", log.LstdFlags)
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
		err = serve(logger)
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
		logger.Fatal(err)
	}
}

func serve(logger *log.Logger) error {
	// The platform's key, or keys while it's changed (comma-separated).
	var keys []ed25519.PublicKey
	for _, text := range strings.Split(os.Getenv("GUILDNET_SANDBOX_PUBLIC_KEY"), ",") {
		key, err := sandbox.PublicKeyFrom(text)
		if err != nil {
			return fmt.Errorf("GUILDNET_SANDBOX_PUBLIC_KEY (from sandboxd keygen): %w", err)
		}
		keys = append(keys, key)
	}
	// Old tokens (a network's name, signed, for ever), only while runners
	// move to new ones.
	acceptV1 := os.Getenv("GUILDNET_SANDBOX_ACCEPT_V1") == "on"
	if acceptV1 {
		logger.Print("taking old sandbox tokens too (GUILDNET_SANDBOX_ACCEPT_V1): turn it off once every runner's been updated")
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
	}
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
	s := sandbox.NewServer(sandbox.Options{Backend: backend, PublicKeys: keys, AcceptV1: acceptV1, Logger: logger, MaxRunning: atOnce})
	logger.Printf("room for %d desks at once, %s each", atOnce, backend.Memory)
	go func() {
		for range time.Tick(time.Minute) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			s.StopIdle(ctx)
			cancel()
		}
	}()
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
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}

func proxy(logger *log.Logger) error {
	allowed := sandbox.DefaultAllowed
	if v := os.Getenv("GUILDNET_DESK_ALLOWED"); v != "" {
		allowed = strings.Split(v, ",")
	}
	// Networks' egress passes, signed by the platform, open their git
	// repositories' hosts too; git servers on the install's own network are
	// reached only where they're named.
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
	var private []string
	for _, h := range strings.Split(os.Getenv("GUILDNET_GIT_PRIVATE_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			private = append(private, h)
		}
	}
	p := &sandbox.Proxy{Allowed: allowed, Keys: keys, Dial: egress.Dial(egress.Options{PrivateHosts: private}), Logger: logger}
	listen := env("GUILDNET_PROXY_LISTEN", ":3128")
	logger.Printf("desks' way out on %s, to %s, and networks' git hosts with their passes (%d keys)", listen, strings.Join(allowed, ", "), len(keys))
	if len(private) > 0 {
		logger.Printf("git servers on this network: %s", strings.Join(private, ", "))
	}
	srv := &http.Server{Addr: listen, Handler: monitor.Handler(p), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	return srv.ListenAndServe()
}
