// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/github"
)

// A customer's runner gets its tokens from the platform's broker, with its
// network's name and secret, and learns from it which account and repos its
// installation has, which change while it runs (the owner connects GitHub,
// or gives it more repos).

type fakeBroker struct {
	srv       *httptest.Server
	connected bool
	repos     []string
	asked     []string
}

func newFakeBroker(t *testing.T) *fakeBroker {
	b := &fakeBroker{}
	mux := http.NewServeMux()
	check := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("GuildNet-Network") != "acme" || r.Header.Get("Authorization") != "Bearer acme-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		if !b.connected {
			w.WriteHeader(http.StatusNotFound)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /github", func(w http.ResponseWriter, r *http.Request) {
		if check(w, r) {
			json.NewEncoder(w).Encode(map[string]any{"account": "acme-inc", "repos": b.repos})
		}
	})
	mux.HandleFunc("POST /github/token", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var in struct {
			Repos []string `json:"repos"`
			Write bool     `json:"write"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		b.asked = append(b.asked, joinRepos(in.Repos))
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_from_broker", "expires_at": time.Now().Add(time.Hour)})
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func TestARunnerGetsTokensFromTheBroker(t *testing.T) {
	fb := newFakeBroker(t)
	fb.connected = true
	b := github.Broker{URL: fb.srv.URL, Network: "acme", Secret: "acme-secret"}

	token, expires, err := b.Token(context.Background(), []string{"web"}, true)

	if err != nil || token != "ghs_from_broker" || expires.IsZero() {
		t.Fatalf("token %q, expires %v, err %v", token, expires, err)
	}
	wrong := github.Broker{URL: fb.srv.URL, Network: "acme", Secret: "nope"}
	if _, _, err := wrong.Token(context.Background(), []string{"web"}, false); err == nil {
		t.Error("a wrong secret should get no token")
	}
}

func TestTheInstallationsReposAreKeptUpToDate(t *testing.T) {
	fb := newFakeBroker(t)
	b := github.Broker{URL: fb.srv.URL, Network: "acme", Secret: "acme-secret"}
	live := &github.Live{}

	if err := live.Refresh(context.Background(), b); err != nil {
		t.Fatalf("not connected yet shouldn't be an error: %v", err)
	}
	if account, repos := live.Get(); account != "" || len(repos) != 0 {
		t.Errorf("before connecting: %q %v", account, repos)
	}
	fb.connected, fb.repos = true, []string{"docs", "web"}
	live.Refresh(context.Background(), b)
	if account, repos := live.Get(); account != "acme-inc" || !slices.Equal(repos, []string{"docs", "web"}) {
		t.Errorf("after connecting: %q %v", account, repos)
	}
}

// A client on a live installation offers tools only once there are repos,
// and only those repos.
func TestAClientFollowsTheLiveInstallation(t *testing.T) {
	f := newFakeGitHub(t, "guildnet")
	live := &github.Live{}
	c, err := github.New(github.Options{Tokens: &elsewhere{f: f}, Live: live, BaseURL: f.URL(),
		TeamRepos: func(string) []string { return []string{"guildnet"} }})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tools(kai)) != 0 {
		t.Error("tools before the network is connected")
	}

	live.Set(owner, []string{"guildnet"})

	if len(c.Tools(kai)) == 0 {
		t.Error("no tools once the installation has the team's repo")
	}
	if _, err := run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x"))); err != nil {
		t.Errorf("opening a PR on the live installation: %v", err)
	}
}
