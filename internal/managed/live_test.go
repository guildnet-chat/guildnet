// SPDX-License-Identifier: AGPL-3.0-only

package managed_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/github"
	"github.com/guildnet-chat/guildnet/internal/managed"
	"github.com/guildnet-chat/guildnet/internal/org"
)

// A live test against the real Managed Agents API. It costs real money
// (cents), so it only runs with GUILDNET_LIVE_MANAGED=1. It uses Haiku, a
// 10-cent desk budget, and archives the agent afterwards.
func TestLiveADeskRunsAToolAndReplies(t *testing.T) {
	if os.Getenv("GUILDNET_LIVE_MANAGED") != "1" {
		t.Skip("live test: set GUILDNET_LIVE_MANAGED=1 to run it (costs a few cents)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sdk, err := managed.NewSDKDesks(ctx, managed.SDKOptions{Network: "livetest", BudgetCents: 10})
	if err != nil {
		t.Fatal(err)
	}
	b := managed.New(sdk, org.NewMemory(org.Seed{}), managed.Options{Company: "A test company.", Owner: "ada"})
	p := config.Persona{Nick: "livetest", Name: "Live Test", Role: "Test agent", Model: "claude-haiku-4-5",
		ReportsTo: "ada", Platform: "managed", Prompt: "You are a test agent. Do exactly what you're asked, briefly."}
	defer b.Fire(context.Background(), p)

	line := chat.Line{Nick: "ada", Text: "livetest: use bash to run `echo guildnet-ok`, then reply with just its output."}
	var steps []string
	start := time.Now()
	res, err := b.Respond(ctx, agent.Request{Persona: p, Where: "#test", Trigger: line, Transcript: []chat.Line{line}},
		func(s agent.Step) { steps = append(steps, s.Summary) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("took %s, cost $%.2f, steps %q, reply %q", time.Since(start).Round(time.Second), res.Cost, steps, res.Reply)

	if !strings.Contains(strings.Join(steps, "|"), "echo guildnet-ok") {
		t.Errorf("steps %q should include running the echo", steps)
	}
	if !strings.Contains(res.Reply, "guildnet-ok") {
		t.Errorf("reply %q should contain the command's output", res.Reply)
	}
	if res.Cost > 0.10 {
		t.Errorf("one tiny task cost $%.2f; expected well under the 10-cent desk budget", res.Cost)
	}
}

// A live end-to-end test: a real desk with the company's repos checked out
// read-only and the GitHub tools. The agent reads the repo, tries to push
// from the shell (which must fail), then asks to merge a throwaway PR; the
// merge pauses for approval and is denied. Costs a few cents, so it only runs
// with GUILDNET_LIVE_MANAGED=1 and GUILDNET_LIVE_GITHUB=1 (plus the
// GUILDNET_GITHUB_* settings). The PR is closed and its branch deleted after.
func TestLiveADeskReadsTheRepoCantPushAndAsksBeforeMerging(t *testing.T) {
	if os.Getenv("GUILDNET_LIVE_MANAGED") != "1" || os.Getenv("GUILDNET_LIVE_GITHUB") != "1" {
		t.Skip("live test: set GUILDNET_LIVE_MANAGED=1, GUILDNET_LIVE_GITHUB=1 and GUILDNET_LIVE_CONFIG (a runner config naming its github repos) to run it (costs a few cents)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cfg, _, err := config.Load(os.Getenv("GUILDNET_LIVE_CONFIG"))
	if err != nil || !cfg.GitHubEnabled() {
		t.Fatalf("GitHub settings: %v", err)
	}
	gh, err := github.New(github.Options{AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID,
		Key: cfg.GitHub.Key, Owner: cfg.GitHub.Owner, Repos: cfg.GitHub.Repos, Teams: cfg.GitHub.Teams})
	if err != nil {
		t.Fatal(err)
	}
	p := config.Persona{Nick: "livetest", Name: "Live Test", Role: "Test agent", Model: "claude-haiku-4-5", Team: "Engineering",
		ReportsTo: "ada", Platform: "managed", Prompt: "You are a test agent. Do exactly what you're asked, in order, briefly."}

	branch := fmt.Sprintf("livetest/desk-%d", time.Now().Unix())
	out, err := gh.Run(ctx, p, "github_open_pull_request", map[string]any{"repo": "guildnet", "branch": branch,
		"title": "GuildNet desk live test (closes itself)", "body": "Opened by a live test; closed straight after.",
		"files": []any{map[string]any{"path": "livetest/desk.txt", "content": "hello from a desk\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	fmt.Sscanf(out[strings.Index(out, "#")+1:], "%d", &n)
	defer func() {
		if err := gh.ClosePullRequest(context.Background(), "guildnet", n); err != nil {
			t.Errorf("cleaning up PR #%d: %v", n, err)
		}
	}()

	sdk, err := managed.NewSDKDesks(ctx, managed.SDKOptions{Network: "livetest", BudgetCents: 15, Repos: gh,
		AllowedHosts: []string{"github.com", "api.github.com", "codeload.github.com"}})
	if err != nil {
		t.Fatal(err)
	}
	b := managed.New(sdk, org.NewMemory(org.Seed{}), managed.Options{Company: "A test company.", Owner: "ada", Toolbox: gh})
	defer b.Fire(context.Background(), p)

	line := chat.Line{Nick: "ada", Text: fmt.Sprintf("livetest: three steps, in order. "+
		"1) Run `head -1 /workspace/guildnet/README.md`. "+
		"2) Run `cd /workspace/guildnet && git push origin HEAD:refs/heads/%s-sneaky` and note whether it worked. "+
		"3) Merge PR #%d in guildnet with github_merge_pull_request. Then reply with the README line and the push result.", branch, n)}
	var steps []string
	start := time.Now()
	res, err := b.Respond(ctx, agent.Request{Persona: p, Where: "#test", Trigger: line, Transcript: []chat.Line{line}},
		func(s agent.Step) { steps = append(steps, s.Summary) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("took %s, cost $%.2f\nsteps %q\nreply %q", time.Since(start).Round(time.Second), res.Cost, steps, res.Reply)
	if res.Approval == nil {
		t.Fatalf("the merge should pause for approval")
	}
	t.Logf("approval: %s", res.Approval.Summary)
	if !strings.Contains(res.Approval.Summary, fmt.Sprintf("merge PR #%d", n)) {
		t.Errorf("approval summary = %q", res.Approval.Summary)
	}

	after, err := b.Resume(ctx, p, res.Approval.Resume, false, "live test: never merge this", func(s agent.Step) { steps = append(steps, s.Summary) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after the denial: cost $%.2f, reply %q", after.Cost, after.Reply)
	if res.Cost+after.Cost > 0.15 {
		t.Errorf("cost $%.2f; expected under the 15-cent desk budget", res.Cost+after.Cost)
	}

	status, _ := gh.Run(ctx, p, "github_pr_status", map[string]any{"repo": "guildnet", "number": float64(n)})
	if !strings.Contains(status, ": open,") {
		t.Errorf("the PR should still be open after the denial: %s", status)
	}
	_, token, _ := gh.Checkout(ctx, p)
	ro, _ := gogithub.NewClient(gogithub.WithAuthToken(token))
	if _, resp, _ := ro.Git.GetRef(ctx, cfg.GitHub.Owner, "guildnet", "heads/"+branch+"-sneaky"); resp == nil || resp.StatusCode != 404 {
		t.Errorf("the push from the shell must fail, but %s-sneaky exists (or couldn't be checked)", branch)
	}
}
