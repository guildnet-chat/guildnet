// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// A network nobody uses can be put to sleep (Core.Sleep: the hosted edition
// does, for its free networks): its IRC server and runner stop, and
// everything is kept. The next visit to its console wakes it, in the few
// seconds that takes; until then it says so. When people (not agents) last
// used each network is noted here: in its console, web chat or an IRC app.

// usedEvery is how often a network's use is written at most.
const usedEvery = time.Hour

// wakeFor is how long a network's IRC server and runner get to start.
const wakeFor = 90 * time.Second

type sleeper struct {
	mu     sync.Mutex
	noted  map[string]time.Time // network -> when its use was last written
	waking map[string]bool
	failed map[string]bool // its last wake-up didn't work
}

// noteUsed notes a person using n now, once an hour at most.
func (p *Platform) noteUsed(ctx context.Context, n Network) {
	now := p.opts.Now()
	p.sleep.mu.Lock()
	if p.sleep.noted == nil {
		p.sleep.noted = map[string]time.Time{}
	}
	last, ok := p.sleep.noted[n.Name]
	if ok && now.Sub(last) < usedEvery && !now.Before(last) {
		p.sleep.mu.Unlock()
		return
	}
	p.sleep.noted[n.Name] = now
	p.sleep.mu.Unlock()
	if err := p.opts.Networks.NoteUsed(ctx, n.Name, now); err != nil {
		monitor.Problem(p.opts.Logger, "noting %s was used: %v", n.Name, err)
	}
}

// Sleep stops n, keeping everything, until someone next opens its console.
func (c *Core) Sleep(ctx context.Context, n Network) error {
	p := c.p
	if p.opts.Host == nil {
		return nil
	}
	if err := p.opts.Host.Stop(ctx, n.Name); err != nil {
		return err
	}
	n.Status, n.Problem, n.Updated = Asleep, "", p.opts.Now()
	if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
		return err
	}
	p.opts.Logger.Printf("%s is asleep", n.Name)
	return nil
}

// waking answers a visit to n, asleep: it starts waking (once, however many
// visit), and says so until it's awake.
func (p *Platform) waking(w http.ResponseWriter, r *http.Request, n Network) {
	p.sleep.mu.Lock()
	if p.sleep.waking == nil {
		p.sleep.waking, p.sleep.failed = map[string]bool{}, map[string]bool{}
	}
	failed := p.sleep.failed[n.Name]
	start := !p.sleep.waking[n.Name]
	p.sleep.waking[n.Name] = true
	p.sleep.mu.Unlock()
	if start {
		p.background(func() { p.wake(n) })
	}
	w.Header().Set("Retry-After", "3")
	w.Header().Set("Cache-Control", "no-store")
	p.renderHome(w, r, http.StatusServiceUnavailable, "waking", withError{View: struct {
		Name   string
		Failed bool
	}{n.Name, failed}})
}

// wake starts n's IRC server and runner, waits for them to answer, and has
// it ready.
func (p *Platform) wake(n Network) {
	defer monitor.Recover()
	ctx, cancel := context.WithTimeout(context.Background(), wakeFor+30*time.Second)
	defer cancel()
	err := p.opts.Host.Start(ctx, n.Name)
	if err == nil {
		err = answers(ctx, n.RunnerURL, n.ErgoURL)
	}
	if err == nil {
		// As it is now: it may have changed while it slept.
		var cur Network
		if cur, err = p.opts.Networks.Network(ctx, n.Name); err == nil && cur.Status == Asleep {
			cur.Status, cur.Updated = Ready, p.opts.Now()
			err = p.opts.Networks.UpdateNetwork(ctx, cur)
		}
	}
	if err == nil {
		p.noteUsed(ctx, n)
		p.opts.Logger.Printf("%s woke up", n.Name)
	} else {
		monitor.Problem(p.opts.Logger, "waking %s: %v", n.Name, err)
	}
	p.sleep.mu.Lock()
	delete(p.sleep.waking, n.Name)
	p.sleep.failed[n.Name] = err != nil
	p.sleep.mu.Unlock()
}

// answers waits until something listens at each address's host and port.
func answers(ctx context.Context, addrs ...string) error {
	for _, a := range addrs {
		u, err := url.Parse(a)
		if err != nil || u.Host == "" {
			continue
		}
		host := u.Host
		if u.Port() == "" {
			port := "80"
			if u.Scheme == "https" || u.Scheme == "wss" {
				port = "443"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
		for {
			c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", host)
			if err == nil {
				c.Close()
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil
}
