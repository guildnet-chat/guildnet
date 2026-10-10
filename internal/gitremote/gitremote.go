// SPDX-License-Identifier: AGPL-3.0-only

// Package gitremote is a network's git repositories on any host, as its
// runner has them from the platform's broker: which there are (kept up to
// date while the runner runs), and a credential to read or push with, asked
// for each time one's needed and never kept.
package gitremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
)

// Repo is one of the network's repositories: whether it has credentials
// to read (none: it's public) and to push with (none: agents only read it).
type Repo struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Read  bool   `json:"read"`
	Write bool   `json:"write"`
}

// Host is the repository's host name.
func (r Repo) Host() string {
	u, err := url.Parse(r.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// HostPort is the repository's host and port (443 unless it says).
func (r Repo) HostPort() string {
	u, err := url.Parse(r.URL)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// Find is the repository named name, in repos.
func Find(repos []Repo, name string) (Repo, bool) {
	i := slices.IndexFunc(repos, func(r Repo) bool { return strings.EqualFold(r.Name, name) })
	if i < 0 {
		return Repo{}, false
	}
	return repos[i], true
}

// Broker is the platform's broker, as the network's runner reaches it.
type Broker struct {
	URL     string
	Network string
	Secret  string // the network's secret
	// HTTP is how the broker is reached; nil means a plain client.
	HTTP *http.Client
}

var brokerClient = &http.Client{Timeout: 30 * time.Second}

func (b Broker) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.URL, "/")+path, &bytes.Buffer{})
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.Secret)
	req.Header.Set("GuildNet-Network", b.Network)
	c := b.HTTP
	if c == nil {
		c = brokerClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("the broker: %s: %s", resp.Status, e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Repos is the network's repositories.
func (b Broker) Repos(ctx context.Context) ([]Repo, error) {
	var out struct {
		Repos []Repo `json:"repos"`
	}
	err := b.do(ctx, "GET", "/git", &out)
	return out.Repos, err
}

// Credential is the credential to read name with (write false) or to push
// to it with, and where it is.
func (b Broker) Credential(ctx context.Context, name string, write bool) (repoURL string, c gitwire.Credential, err error) {
	access := "read"
	if write {
		access = "write"
	}
	var out struct {
		URL   string `json:"url"`
		User  string `json:"user"`
		Token string `json:"token"`
	}
	if err := b.do(ctx, "POST", "/git/"+url.PathEscape(name)+"/"+access, &out); err != nil {
		return "", c, err
	}
	return out.URL, gitwire.Credential{User: out.User, Token: out.Token}, nil
}

// Checkouts is what a desk checks out of the network's repositories: the
// ones its agent's team works on (git:<name> in its repos), each with the
// credential to read it, asked of the broker each time. A repository the
// network no longer has is left out.
type Checkouts struct {
	Live      *Live
	TeamRepos func(team string) []string
}

// Clones is p's desk's repositories.
func (c Checkouts) Clones(ctx context.Context, p config.Persona) ([]desk.Clone, error) {
	repos := c.Live.Repos()
	var out []desk.Clone
	for _, name := range c.TeamRepos(p.Team) {
		name, ok := strings.CutPrefix(name, "git:")
		if !ok {
			continue
		}
		repo, ok := Find(repos, name)
		if !ok {
			continue
		}
		clone := desk.Clone{Name: repo.Name, URL: repo.URL}
		if repo.Read {
			addr, cred, err := c.Live.Broker.Credential(ctx, repo.Name, false)
			if err != nil {
				return nil, fmt.Errorf("%s's credential: %w", repo.Name, err)
			}
			clone.URL, clone.Read = addr, cred
		}
		out = append(out, clone)
	}
	return out, nil
}

// Live is the network's repositories as last heard from the broker, asked
// again every few minutes (and whenever one's named that isn't known).
type Live struct {
	Broker Broker
	mu     sync.Mutex
	repos  []Repo
}

// Repos is the repositories, as last heard.
func (l *Live) Repos() []Repo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.repos)
}

// Refresh asks the broker again.
func (l *Live) Refresh(ctx context.Context) error {
	repos, err := l.Broker.Repos(ctx)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.repos = repos
	return nil
}
