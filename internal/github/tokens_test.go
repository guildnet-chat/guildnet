// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/github"
)

// Where tokens come from. Our own network mints them with the app's key; a
// customer's runner never holds the key (it could mint tokens for every
// customer), so it asks the platform's broker instead. Either way the tools
// work with tokens limited to the network's repos and to what they need.

// elsewhere is a token source that isn't the app's key: here it gets its
// tokens from the fake GitHub the way the platform's broker would.
type elsewhere struct {
	f     *fakeGitHub
	asked []string // "repos read|write"
}

func (e *elsewhere) Token(ctx context.Context, repos []string, write bool) (string, time.Time, error) {
	perms := map[string]string{"contents": "read", "metadata": "read"}
	mode := "read"
	if write {
		perms, mode = map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"}, "write"
	}
	e.asked = append(e.asked, joinRepos(repos)+" "+mode)
	body, _ := json.Marshal(map[string]any{"repositories": repos, "permissions": perms})
	req, _ := http.NewRequestWithContext(ctx, "POST", e.f.URL()+"app/installations/2/access_tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer broker")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Token, out.ExpiresAt, nil
}

func joinRepos(repos []string) string {
	s := slices.Clone(repos)
	slices.Sort(s)
	b, _ := json.Marshal(s)
	return string(b)
}

func TestTokensCanComeFromSomewhereOtherThanTheAppsKey(t *testing.T) {
	f := newFakeGitHub(t, "guildnet", "guildnet-chat")
	src := &elsewhere{f: f}
	c, err := github.New(github.Options{Tokens: src, Owner: owner, Repos: []string{"guildnet", "guildnet-chat"}, BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x"))); err != nil {
		t.Fatal(err)
	}
	if _, token, err := c.Checkout(context.Background(), kai); err != nil || token == "" {
		t.Fatalf("checkout: %q, %v", token, err)
	}

	if !slices.Contains(src.asked, `["guildnet","guildnet-chat"] write`) || !slices.Contains(src.asked, `["guildnet","guildnet-chat"] read`) {
		t.Errorf("asked for %v, want a write token for the tools and a read-only one for the workspace, both for just the network's repos", src.asked)
	}
}

// With the app's own key too, the tools' token is limited to the network's
// repos and to what the tools need: not the installation's every permission.
func TestTheToolsTokenIsLimitedToTheReposAndWhatTheToolsNeed(t *testing.T) {
	c, f := newGitHub(t)

	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))

	g := f.firstGrant()
	if joinRepos(g.Repositories) != `["guildnet","guildnet-chat"]` || g.Permissions["contents"] != "write" || g.Permissions["pull_requests"] != "write" {
		t.Errorf("the tools' token = %+v, want write access to just the network's repos", g)
	}
	if _, ok := g.Permissions["workflows"]; ok {
		t.Error("the tools' token mustn't be able to change workflows")
	}
}

// An app whose installation wasn't granted an optional permission (reading
// commit statuses, say) still gets its tools' tokens, without it.
func TestAToolsTokenAsksOnlyForWhatTheInstallationHas(t *testing.T) {
	f := newFakeGitHub(t, "guildnet")
	f.noStatuses = true
	c, err := github.New(github.Options{AppID: 1, InstallationID: 2, Key: testKey(t), Owner: owner, Repos: []string{"guildnet"}, BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x"))); err != nil {
		t.Fatalf("opening a PR without the statuses permission: %v", err)
	}
	if _, ok := f.firstGrant().Permissions["statuses"]; ok {
		t.Error("asked for statuses, which the installation doesn't have")
	}
}

// Which repos an agent works on comes from its team, as the org says; a
// repo outside the network's is never given, whatever the team says.
func TestAnAgentsReposComeFromItsTeam(t *testing.T) {
	f := newFakeGitHub(t, "guildnet", "guildnet-chat")
	teams := map[string][]string{"Engineering": {"guildnet", "secret-plans"}}
	c, err := github.New(github.Options{AppID: 1, InstallationID: 2, Key: testKey(t), Owner: owner, Repos: []string{"guildnet", "guildnet-chat"},
		BaseURL: f.URL(), TeamRepos: func(team string) []string { return teams[team] }})
	if err != nil {
		t.Fatal(err)
	}
	engineer, marketer := kai, kai
	engineer.Team, marketer.Team = "Engineering", "Marketing"

	if urls, _, _ := c.Checkout(context.Background(), engineer); len(urls) != 1 || urls[0] != "https://github.com/"+owner+"/guildnet" {
		t.Errorf("an engineer checks out %v, want just guildnet", urls)
	}
	if urls, _, _ := c.Checkout(context.Background(), marketer); len(urls) != 0 {
		t.Errorf("marketing, with no repos, checks out %v", urls)
	}
	if tools := c.Tools(marketer); len(tools) != 0 {
		t.Errorf("marketing gets %d GitHub tools, want none", len(tools))
	}
}
