// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/monitor"
)

// A network's desks' files, in all, count against its storage (sandboxd
// limits them). Its owner sees how much is used in Settings, each agent's
// page says what its own desk holds, whoever may change an agent can clear
// its desk, and the owner is told as the desks pass 80% and as they fill.
// Pages never wait on sandboxd (its VM may be off, and asking never wakes
// it): they show what it last said. The warnings' job asks while the VM is
// on, and a page showing what's gone stale asks again in the background,
// if sandboxd answered lately.

const (
	// storageFresh is how long what sandboxd said is shown without asking
	// again.
	storageFresh = 2 * time.Minute
	// storageCheckEvery is how often the owner's warnings are checked for.
	storageCheckEvery = 10 * time.Minute
	// storageNearly is how full, in percent, the owner's first warning
	// comes at; the second is at 100.
	storageNearly = 80
)

// deskStorage is networks' desks' storage as sandboxd last said, by
// network, and when it last answered.
type deskStorage struct {
	mu       sync.Mutex
	seen     map[string]storageSeen
	asking   map[string]bool // asked in the background now
	answered time.Time
}

type storageSeen struct {
	s  sandbox.Storage
	at time.Time
}

// storageOf is n's desks' storage as sandboxd last said, against its plan's
// limit where the extension sets one; ok is false if it isn't known. Gone
// stale, it's asked again in the background, if sandboxd answered lately.
func (p *Platform) storageOf(ctx context.Context, n Network) (sandbox.Storage, bool) {
	s := p.opts.Sandbox
	if s.URL == "" || len(s.Key) == 0 {
		return sandbox.Storage{}, false
	}
	key := strings.ToLower(n.Name)
	now := p.opts.Now()
	p.storage.mu.Lock()
	seen, known := p.storage.seen[key]
	ask := (!known || now.Sub(seen.at) >= storageFresh) && now.Sub(p.storage.answered) < storageCheckEvery && !p.storage.asking[key]
	if ask {
		if p.storage.asking == nil {
			p.storage.asking = map[string]bool{}
		}
		p.storage.asking[key] = true
	}
	p.storage.mu.Unlock()
	if ask {
		p.background(func() {
			defer monitor.Recover()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p.askStorage(ctx, n.Name)
			p.storage.mu.Lock()
			delete(p.storage.asking, key)
			p.storage.mu.Unlock()
		})
	}
	if !known {
		return sandbox.Storage{}, false
	}
	return p.planned(ctx, n, seen.s), true
}

// askStorage asks sandboxd for network's desks' storage now, keeping what
// it says; false if it didn't answer.
func (p *Platform) askStorage(ctx context.Context, network string) bool {
	got, err := p.sandboxd(network).Storage(ctx)
	if err != nil {
		if !errors.Is(err, desk.ErrUnreachable) {
			monitor.Problem(p.opts.Logger, "%s's desks' storage: %v", network, err)
		}
		return false
	}
	p.storage.mu.Lock()
	defer p.storage.mu.Unlock()
	if p.storage.seen == nil {
		p.storage.seen = map[string]storageSeen{}
	}
	p.storage.seen[strings.ToLower(network)] = storageSeen{s: got, at: p.opts.Now()}
	p.storage.answered = p.opts.Now()
	return true
}

// planned is st with n's plan's limit, where the extension sets one: what
// its desks are held to from their next use.
func (p *Platform) planned(ctx context.Context, n Network, st sandbox.Storage) sandbox.Storage {
	if p.hooks.Desks != nil {
		if l, err := p.hooks.Desks.Limits(ctx, n, ""); err == nil && l.StorageGB > 0 {
			st.LimitGB = l.StorageGB
		}
	}
	return st
}

// sandboxd is network's desks in sandboxd, with the platform's own token,
// good for a minute.
func (p *Platform) sandboxd(network string) *desk.Remote {
	s := p.opts.Sandbox
	sd := desk.NewRemote(s.URL, sandbox.TokenFor(s.Key, network, sandbox.Admin, p.opts.Now(), time.Minute))
	if s.Through != nil {
		sd.Through(s.Through)
	}
	return sd
}

// percentFull is how much of st's limit is used, in percent; 0 with no
// limit.
func percentFull(st sandbox.Storage) int {
	if st.LimitGB <= 0 {
		return 0
	}
	return int(st.Used() * 100 / (int64(st.LimitGB) << 30))
}

// gbText is bytes in GB, to a tenth: "3.2".
func gbText(bytes int64) string {
	return fmt.Sprintf("%.1f", float64(bytes)/(1<<30))
}

// storageView is a network's desks' storage as Settings shows it.
type storageView struct {
	Used    string // "3.2"
	LimitGB int    // 0: no limit
	Percent int
}

func (p *Platform) storageViewOf(ctx context.Context, n Network) *storageView {
	st, ok := p.storageOf(ctx, n)
	if !ok {
		return nil
	}
	return &storageView{Used: gbText(st.Used()), LimitGB: st.LimitGB, Percent: percentFull(st)}
}

// deskOf is what agent nick's desk holds ("1.5"), if it has one there.
func (p *Platform) deskOf(ctx context.Context, n Network, nick string) (string, bool) {
	st, ok := p.storageOf(ctx, n)
	if !ok {
		return "", false
	}
	b, has := st.Desks[strings.ToLower(nick)]
	return gbText(b), has
}

// clearDesk deletes an agent's desk's files, for whoever may change the
// agent: it starts afresh at its next wake-up (its repos checked out again).
func (p *Platform) clearDesk(w http.ResponseWriter, r *http.Request) {
	nick := r.PathValue("nick")
	v, err := p.loadAgent(r, nick)
	if err != nil {
		p.showAgentError(w, r, statusOf(err), messageOf(err))
		return
	}
	if !v.CanEdit {
		p.showAgentError(w, r, http.StatusForbidden, "Only someone who may change "+v.Agent.Nick+" clears its desk.")
		return
	}
	if p.opts.Sandbox.URL == "" {
		p.showAgentError(w, r, http.StatusNotFound, v.Agent.Nick+" has no desk here.")
		return
	}
	n := networkOf(r)
	if !p.sandboxUp(r.Context()) {
		p.wakeSandbox(n.Name)
		p.showAgentError(w, r, http.StatusServiceUnavailable, "The desks are waking up: try again in a minute.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	// A desks token, as its runner's, saying the plan's storage as theirs do.
	var gb int
	if p.hooks.Desks != nil {
		if l, err := p.hooks.Desks.Limits(ctx, n, ""); err == nil {
			gb = l.StorageGB
		}
	}
	s := p.opts.Sandbox
	sd := desk.NewRemote(s.URL, sandbox.DesksTokenFor(s.Key, n.Name, gb, p.opts.Now(), time.Minute))
	if s.Through != nil {
		sd.Through(s.Through)
	}
	if err := sd.Remove(ctx, v.Agent.Nick); err != nil {
		if errors.Is(err, desk.ErrUnreachable) {
			p.wakeSandbox(n.Name)
			p.showAgentError(w, r, http.StatusServiceUnavailable, "The desks are waking up: try again in a minute.")
			return
		}
		monitor.Problem(p.opts.Logger, "%s: clearing %s's desk: %v", n.Name, v.Agent.Nick, err)
		p.showAgentError(w, r, http.StatusBadGateway, "Couldn't clear the desk. Try again in a moment.")
		return
	}
	p.askStorage(ctx, n.Name)
	who, _ := sessionOf(r)
	p.opts.Logger.Printf("%s: %s cleared %s's desk", n.Name, who.Account, v.Agent.Nick)
	http.Redirect(w, r, "/staff/"+url.PathEscape(v.Agent.Nick), http.StatusSeeOther)
}

// watchDeskStorage tells each network's owner as its desks pass 80% of its
// storage, and as they fill: once each, until use drops back below 80%.
// Only while the sandbox VM is on (nothing changes while it sleeps).
func (p *Platform) watchDeskStorage(ctx context.Context) {
	defer monitor.Recover()
	if p.opts.Networks == nil || !p.sandboxUp(ctx) {
		return
	}
	networks, err := p.opts.Networks.AllNetworks(ctx)
	if err != nil {
		monitor.Problem(p.opts.Logger, "checking desks' storage: %v", err)
		return
	}
	for _, n := range networks {
		if n.Status != Ready && n.Status != Asleep {
			continue
		}
		asking, cancel := context.WithTimeout(ctx, 30*time.Second) // du of a big desk takes a while
		answered := p.askStorage(asking, n.Name)
		cancel()
		if !answered {
			continue
		}
		st, _ := p.storageOf(ctx, n)
		level := 0
		switch pct := percentFull(st); {
		case pct >= 100:
			level = 100
		case pct >= storageNearly:
			level = storageNearly
		}
		if level == n.StorageTold {
			continue
		}
		if level > n.StorageTold {
			p.tellOwner(ctx, n, storageEmail(n, p.scheme()+"://"+n.Name+"."+p.opts.BaseDomain, st, level))
		}
		if err := p.opts.Networks.NoteStorageTold(ctx, n.Name, level); err != nil {
			monitor.Problem(p.opts.Logger, "%s: noting its desks' storage was told: %v", n.Name, err)
		}
	}
}

func storageEmail(n Network, origin string, st sandbox.Storage, level int) Letter {
	holds := fmt.Sprintf("%s's desks hold %s of %d GB (%d%%).", n.Name, gbText(st.Used()), st.LimitGB, percentFull(st))
	subject, say := n.Name+"'s desks' storage is nearly full", holds+" Once it's full, agents can't save files at their desks."
	if level >= 100 {
		subject, say = n.Name+"'s desks' storage is full", holds+" Agents can't save files at their desks until some space is freed."
	}
	return Letter{Subject: subject, Heading: subject,
		Paras:  []string{say, "To free some, clear a desk you don't need from its agent's page in the console (its work and home folders go), or ask an agent to delete files it's finished with."},
		Button: "Open Settings", Link: origin + "/settings",
		Why: fmt.Sprintf(ownerWhy, n.Name)}
}
