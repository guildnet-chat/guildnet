// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/github"
)

// liveConfig is the runner config live tests use: GUILDNET_LIVE_CONFIG, a
// config whose github section names the repos (the sample's has none), with
// the app's GUILDNET_GITHUB_* settings. It skips unless GUILDNET_LIVE_GITHUB=1.
func liveConfig(t *testing.T) *config.Runner {
	t.Helper()
	if os.Getenv("GUILDNET_LIVE_GITHUB") != "1" {
		t.Skip("live test: set GUILDNET_LIVE_GITHUB=1, GUILDNET_LIVE_CONFIG and the GUILDNET_GITHUB_* settings to run it (free, but it opens and closes a real PR)")
	}
	cfg, _, err := config.Load(os.Getenv("GUILDNET_LIVE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GitHubEnabled() || len(cfg.GitHub.Repos) == 0 {
		t.Fatal("GUILDNET_LIVE_CONFIG has no github repos, or the GUILDNET_GITHUB_* settings are missing")
	}
	return cfg
}

// LiveClient is the real GitHub App, from liveConfig, for live tests.
func LiveClient(t *testing.T) *github.Client {
	t.Helper()
	cfg := liveConfig(t)
	c, err := github.New(github.Options{AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID,
		Key: cfg.GitHub.Key, Owner: cfg.GitHub.Owner, Repos: cfg.GitHub.Repos})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Against the real GitHub: open a PR on a throwaway branch, check its
// status, comment, ready a merge for approval (without merging), confirm the
// workspace token can't write, then close the PR and delete the branch.
func TestLiveThePullRequestFlowWorksOnRealGitHub(t *testing.T) {
	c := LiveClient(t)
	cfg := liveConfig(t)
	owner, repo := cfg.GitHub.Owner, cfg.GitHub.Repos[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := config.Persona{Nick: "livetest", Name: "Live Test", Role: "test agent"}
	branch := fmt.Sprintf("livetest/%d", time.Now().Unix())

	out, err := c.Run(ctx, p, "github_open_pull_request", map[string]any{
		"repo": repo, "branch": branch, "title": "GuildNet live test (closes itself)",
		"body":  "Opened by GuildNet's live test; it closes this and deletes the branch straight away.",
		"files": []any{map[string]any{"path": "livetest/" + branch[9:] + ".txt", "content": "hello from the live test\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out)
	var n int
	if _, err := fmt.Sscanf(out[strings.Index(out, "#")+1:], "%d", &n); err != nil || n == 0 {
		t.Fatalf("couldn't find the PR number in %q", out)
	}
	defer func() {
		if err := c.ClosePullRequest(context.Background(), repo, n); err != nil {
			t.Errorf("cleaning up PR #%d: %v", n, err)
		}
	}()

	status, err := c.Run(ctx, p, "github_pr_status", map[string]any{"repo": repo, "number": float64(n)})
	if err != nil || !strings.Contains(status, "GuildNet live test") {
		t.Errorf("status = %q, %v", status, err)
	}
	t.Log(status)
	if _, err := c.Run(ctx, p, "github_comment", map[string]any{"repo": repo, "number": float64(n), "body": "Live test comment."}); err != nil {
		t.Error(err)
	}
	summary, pinned, err := c.Prepare(ctx, p, "github_merge_pull_request", map[string]any{"repo": repo, "number": float64(n)})
	if err != nil || pinned["head_sha"] == "" {
		t.Errorf("preparing the merge: %q %v %v", summary, pinned, err)
	}
	t.Log("approval would say: " + summary)

	// The workspace's token must not be able to write.
	_, token, err := c.Checkout(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	ro, _ := gh.NewClient(gh.WithAuthToken(token))
	if _, _, err := ro.Git.CreateRef(ctx, owner, repo, gh.CreateRef{Ref: "refs/heads/" + branch + "-sneaky", SHA: fmt.Sprint(pinned["head_sha"])}); err == nil {
		ro.Git.DeleteRef(ctx, owner, repo, "heads/"+branch+"-sneaky")
		t.Error("the workspace token could create a branch; it must be read-only")
	} else {
		t.Logf("workspace token write refused, as it should be: %v", err)
	}
	if _, _, err := ro.Repositories.Get(ctx, owner, repo); err != nil {
		t.Errorf("the workspace token should still read the repo: %v", err)
	}
}

// Every team's workspace token can be minted: the app is installed on every
// repo the config gives a team.
func TestLiveEveryTeamsReposAreReachable(t *testing.T) {
	cfg := liveConfig(t)
	c, _ := github.New(github.Options{AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID,
		Key: cfg.GitHub.Key, Owner: cfg.GitHub.Owner, Repos: cfg.GitHub.Repos, Teams: cfg.GitHub.Teams})
	for team := range cfg.GitHub.Teams {
		urls, token, err := c.Checkout(context.Background(), config.Persona{Nick: "livetest", Team: team})
		if err != nil || token == "" {
			t.Errorf("%s: %v (is the GitHub App installed on %v?)", team, err, urls)
			continue
		}
		t.Logf("%s: %v", team, urls)
	}
}
