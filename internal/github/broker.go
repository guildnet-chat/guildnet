// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Broker is the platform's GitHub token broker, as a customer's runner uses
// it: tokens for its own network's installation, asked for with the
// network's name and secret. The app's key never leaves the platform.
type Broker struct {
	URL     string // e.g. https://platform:8082
	Network string
	Secret  string // the network's secret
	// HTTP is how the broker is reached (one trusting GuildNet's own CA, for
	// TLS inside the VM); nil means a plain client.
	HTTP *http.Client
}

var brokerClient = &http.Client{Timeout: 30 * time.Second}

func (b Broker) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return brokerClient
}

func (b Broker) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body bytes.Buffer
	if in != nil {
		json.NewEncoder(&body).Encode(in)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.URL, "/")+path, &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+b.Secret)
	req.Header.Set("GuildNet-Network", b.Network)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("github broker: %s", resp.Status)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// Token asks the broker for a token for repos.
func (b Broker) Token(ctx context.Context, repos []string, write bool) (string, time.Time, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := b.do(ctx, "POST", "/github/token", map[string]any{"repos": repos, "write": write}, &out); err != nil {
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

// Installation is the account and repos the network's installation has;
// connected false when the network isn't connected to GitHub.
func (b Broker) Installation(ctx context.Context) (account string, repos []string, connected bool, err error) {
	var out struct {
		Account string   `json:"account"`
		Repos   []string `json:"repos"`
	}
	status, err := b.do(ctx, "GET", "/github", nil, &out)
	if status == http.StatusNotFound {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	return out.Account, out.Repos, true, nil
}

// Live is an installation's account and repos, kept up to date while the
// runner runs: the owner may connect GitHub, or give it more repos, at any
// time.
type Live struct {
	mu      sync.Mutex
	account string
	repos   []string
}

// Get is the account and repos, as last heard.
func (l *Live) Get() (account string, repos []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.account, slices.Clone(l.repos)
}

// Set replaces them.
func (l *Live) Set(account string, repos []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.account, l.repos = account, slices.Clone(repos)
}

// Refresh asks the broker again. Not being connected (yet) isn't an error:
// it's no account and no repos.
func (l *Live) Refresh(ctx context.Context, b Broker) error {
	account, repos, _, err := b.Installation(ctx)
	if err != nil {
		return err
	}
	l.Set(account, repos)
	return nil
}
