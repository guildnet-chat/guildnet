// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Sandbox is where desks of GuildNet's own are: sandboxd on the sandbox VM.
type Sandbox struct {
	// URL is sandboxd's, as networks' runners reach it.
	URL string
	// Key signs each network's token; sandboxd has the public half.
	Key ed25519.PrivateKey
	// Wake asks for the sandbox VM to be started (it powers off when idle);
	// nil if it never sleeps.
	Wake func() error
	// Through is how sandboxd is reached (over TLS from GuildNet's own CA,
	// where it serves that); nil for plain HTTP.
	Through http.RoundTripper
}

// wakeEvery is how often the sandbox VM is asked to start, however many
// runners want it.
const wakeEvery = 30 * time.Second

// sandboxWakes remembers when the VM was last asked to start.
type sandboxWakes struct {
	mu   sync.Mutex
	last time.Time
}

// brokerSandbox tells a network's runner where sandboxd is, with its
// network's token.
func (p *Platform) brokerSandbox(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	s := p.opts.Sandbox
	if s.URL == "" || len(s.Key) == 0 {
		brokerError(w, http.StatusNotFound, "there's no sandbox here, so no desks")
		return
	}
	// The network's storage, its plan's, which sandboxd limits its desks'
	// files to. Without it, no token: one saying nothing would leave the
	// network at sandboxd's default.
	var storage int
	if p.hooks.Desks != nil {
		l, err := p.hooks.Desks.Limits(r.Context(), n, "")
		if err != nil {
			p.opts.Logger.Printf("%s's desk limits: %v", n.Name, err)
			brokerError(w, http.StatusServiceUnavailable, "couldn't find the network's storage: try again")
			return
		}
		storage = l.StorageGB
	}
	now := p.opts.Now()
	out := map[string]any{"url": s.URL,
		"token": sandbox.DesksTokenFor(s.Key, n.Name, storage, now, deskTokenTTL), "expires": now.Add(deskTokenTTL).UTC().Format(time.RFC3339)}
	// The network's egress pass: its desks hand it to their proxy to reach
	// its git repositories' hosts, and only its own.
	if hosts := p.gitHosts(r.Context(), n.Name); len(hosts) > 0 {
		out["egress"] = sandbox.PassFor(s.Key, n.Name, hosts, now, deskTokenTTL)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, out)
}

// deskTokenTTL is how long a runner's desk token is good for: it asks for
// another before then, so one that leaks isn't any use for long.
const deskTokenTTL = 15 * time.Minute

// brokerSandboxWake starts the sandbox VM for a runner whose desk can't
// reach it.
func (p *Platform) brokerSandboxWake(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	if p.opts.Sandbox.Wake == nil {
		brokerError(w, http.StatusNotFound, "the sandbox doesn't sleep here")
		return
	}
	if err := p.wakeSandbox(n.Name); err != nil {
		brokerError(w, http.StatusBadGateway, "couldn't wake the sandbox")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// wakeSandbox asks for the sandbox VM to be started, for network, unless it
// was asked lately.
func (p *Platform) wakeSandbox(network string) error {
	if p.opts.Sandbox.Wake == nil {
		return nil
	}
	p.sandboxWakes.mu.Lock()
	due := p.opts.Now().Sub(p.sandboxWakes.last) >= wakeEvery
	if due {
		p.sandboxWakes.last = p.opts.Now()
	}
	p.sandboxWakes.mu.Unlock()
	if !due {
		return nil
	}
	if err := p.opts.Sandbox.Wake(); err != nil {
		monitor.Problem(p.opts.Logger, "waking the sandbox for %s: %v", network, err)
		return err
	}
	p.opts.Logger.Printf("woke the sandbox for %s", network)
	return nil
}

// A deleted network's desks go with it. Until they have, it keeps its name
// (serving nothing, with desksLeft as its problem): a network made later
// with the same name would otherwise get them, files and all.

const desksLeft = "Its desks are still being removed."

// removeDesksTimeout is how long removing a network's desks may take.
const removeDesksTimeout = 2 * time.Minute

// removeDesks deletes every desk of network's in sandboxd. If the sandbox
// is asleep, it's woken, and this fails: try again once it's up.
func (p *Platform) removeDesks(ctx context.Context, network string) error {
	s := p.opts.Sandbox
	if s.URL == "" || len(s.Key) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, removeDesksTimeout)
	defer cancel()
	// Only the platform's own token, good for the job's length, removes
	// every desk: never one a runner was given.
	sd := desk.NewRemote(s.URL, sandbox.TokenFor(s.Key, network, sandbox.Admin, p.opts.Now(), removeDesksTimeout))
	if s.Through != nil {
		sd.Through(s.Through)
	}
	err := sd.RemoveAll(ctx)
	if errors.Is(err, desk.ErrUnreachable) {
		p.wakeSandbox(network)
	}
	if err != nil {
		return err
	}
	p.opts.Logger.Printf("network %s: its desks removed", network)
	return nil
}

// forgetNetwork removes a deleted network's desks, then the network: its
// name is free again.
func (p *Platform) forgetNetwork(ctx context.Context, n Network) error {
	if err := p.removeDesks(ctx, n.Name); err != nil {
		if n.Problem != desksLeft {
			n.Problem, n.Updated = desksLeft, p.opts.Now()
			if uerr := p.opts.Networks.UpdateNetwork(ctx, n); uerr != nil {
				monitor.Problem(p.opts.Logger, "deleting %s: %v", n.Name, uerr)
			}
		}
		if errors.Is(err, desk.ErrUnreachable) {
			p.opts.Logger.Printf("network %s: its desks wait for the sandbox to wake", n.Name)
		} else {
			monitor.Problem(p.opts.Logger, "removing %s's desks: %v", n.Name, err)
		}
		return err
	}
	if err := p.opts.Networks.RemoveNetwork(ctx, n.Name); err != nil {
		monitor.Problem(p.opts.Logger, "forgetting %s: %v", n.Name, err)
		return err
	}
	return nil
}

// SweepDesks finishes deleting networks whose desks couldn't be removed at
// the time (the sandbox was asleep). Run every few minutes.
func (p *Platform) SweepDesks(ctx context.Context) {
	defer monitor.Recover()
	if p.opts.Networks == nil {
		return
	}
	networks, err := p.opts.Networks.AllNetworks(ctx)
	if err != nil {
		monitor.Problem(p.opts.Logger, "finding networks whose desks are left: %v", err)
		return
	}
	for _, n := range networks {
		if n.Status == Deleting && n.Problem == desksLeft {
			p.forgetNetwork(ctx, n)
		}
	}
}

// sandboxCertWarn: this long before sandboxd's certificate runs out, the
// console says it needs a new one. A desks host of its own keeps the
// certificate it was given; one beside the console is renewed at 30 days,
// so is never warned about.
const sandboxCertWarn = 21 * 24 * time.Hour

// sandboxdCertificate is when the certificate sandboxd serves runs out, as
// it served it just now (zero over plain HTTP), or why it didn't answer.
func (p *Platform) sandboxdCertificate(ctx context.Context) (time.Time, error) {
	s := p.opts.Sandbox
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.URL, "/")+"/v1/idle", nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := (&http.Client{Transport: s.Through}).Do(req)
	if err != nil {
		return time.Time{}, err
	}
	resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return time.Time{}, nil
	}
	return resp.TLS.PeerCertificates[0].NotAfter, nil
}

// checkSandboxdCertificate logs, from three weeks before sandboxd's
// certificate runs out, that it needs a new one.
func (p *Platform) checkSandboxdCertificate(ctx context.Context) {
	if p.opts.Sandbox.URL == "" {
		return
	}
	until, err := p.sandboxdCertificate(ctx)
	if err == nil && !until.IsZero() && until.Sub(p.opts.Now()) < sandboxCertWarn {
		monitor.Problem(p.opts.Logger, "sandboxd's certificate runs out on %s: issue it a new one (docs/self-host/desks-host.md)", until.UTC().Format("2 January 2006"))
	}
}
