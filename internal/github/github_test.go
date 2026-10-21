// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/github"
)

// GuildNet's GitHub tools: agents read code in their workspace with a
// read-only token, and change it only through these tools, which the runner
// carries out with the app's own credentials. Merging waits for a person.

var kai = config.Persona{Nick: "kai", Name: "Kai", Role: "Backend engineer"}

func newGitHub(t *testing.T) (*github.Client, *fakeGitHub) {
	t.Helper()
	f := newFakeGitHub(t, "guildnet", "guildnet-chat", "secret-plans")
	c, err := github.New(github.Options{AppID: 1, InstallationID: 2, Key: testKey(t), Owner: owner,
		Repos: []string{"guildnet", "guildnet-chat"}, Bot: fakeBot, BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func run(t *testing.T, c *github.Client, tool string, input map[string]any) (string, error) {
	t.Helper()
	return c.Run(context.Background(), kai, tool, input)
}

func openPR(branch string, files ...any) map[string]any {
	return map[string]any{"repo": "guildnet", "branch": branch, "title": "Add a health check",
		"body": "So the load balancer knows we're up.", "message": "Add /healthz", "files": files}
}

func file(path, content string) map[string]any {
	return map[string]any{"path": path, "content": content}
}

func TestOpeningAPullRequestCommitsTheFilesOnANewBranch(t *testing.T) {
	c, f := newGitHub(t)

	out, err := run(t, c, "github_open_pull_request", openPR("kai/healthz",
		file("health.go", "package main\n// healthz\n"),
		map[string]any{"path": "main.go", "delete": true}))

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.file("guildnet", "kai/healthz", "health.go"); got != "package main\n// healthz\n" {
		t.Errorf("health.go on the branch = %q", got)
	}
	if _, still := f.file("guildnet", "kai/healthz", "main.go"); still {
		t.Error("main.go should be deleted on the branch")
	}
	if got, _ := f.file("guildnet", "kai/healthz", "README.md"); got == "" {
		t.Error("files the agent didn't touch should be kept")
	}
	if _, onMain := f.file("guildnet", "main", "health.go"); onMain {
		t.Error("main must not change")
	}
	pulls := f.pulls("guildnet")
	if len(pulls) != 1 || pulls[0].Head != "kai/healthz" || pulls[0].Base != "main" || pulls[0].Title != "Add a health check" {
		t.Fatalf("pull requests = %+v", pulls)
	}
	if !strings.Contains(out, "#1") || !strings.Contains(out, "https://github.com/") {
		t.Errorf("the agent should be told the PR number and link, got %q", out)
	}
}

func TestCommitsAndPullRequestsSayWhichAgentMadeThem(t *testing.T) {
	c, f := newGitHub(t)

	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))

	head := f.head("guildnet", "kai/healthz")
	if !strings.Contains(head.Author, "Kai") || head.Message != "Add /healthz" {
		t.Errorf("commit = %+v, want Kai as author and the agent's message", head)
	}
	if body := f.pulls("guildnet")[0].Body; !strings.Contains(body, "kai") || !strings.Contains(body, "So the load balancer") {
		t.Errorf("PR body %q should keep the agent's text and say kai opened it", body)
	}
}

func TestPushingToTheSameBranchAddsToTheOpenPullRequest(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "v1")))

	out, err := run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "v2")))

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.file("guildnet", "kai/healthz", "health.go"); got != "v2" {
		t.Errorf("health.go = %q, want the second version", got)
	}
	if n := len(f.pulls("guildnet")); n != 1 {
		t.Errorf("%d pull requests, want the existing one updated", n)
	}
	if !strings.Contains(out, "#1") {
		t.Errorf("the agent should be told it updated PR #1, got %q", out)
	}
	if parents := f.head("guildnet", "kai/healthz").Parents; len(parents) != 1 || parents[0] == "c0" {
		t.Errorf("the new commit should build on the branch, not on main; parents %v", parents)
	}
}

func TestAgentsCantCommitStraightToTheDefaultBranch(t *testing.T) {
	c, f := newGitHub(t)

	_, err := run(t, c, "github_open_pull_request", openPR("main", file("health.go", "x")))

	if err == nil {
		t.Error("committing to main should be refused")
	}
	if _, onMain := f.file("guildnet", "main", "health.go"); onMain {
		t.Error("main changed")
	}
}

func TestOnlyTheCompanysChosenReposCanBeTouched(t *testing.T) {
	c, f := newGitHub(t)
	in := openPR("kai/x", file("a.txt", "a"))
	in["repo"] = "secret-plans"

	_, err := run(t, c, "github_open_pull_request", in)

	if err == nil || !strings.Contains(err.Error(), "guildnet") {
		t.Errorf("err = %v, want a refusal that lists the repos it can use", err)
	}
	if len(f.pulls("secret-plans")) != 0 {
		t.Error("a PR was opened on a repo outside the list")
	}
}

func TestReposCanBeNamedWithOrWithoutTheOwner(t *testing.T) {
	c, f := newGitHub(t)
	in := openPR("kai/x", file("a.txt", "a"))
	in["repo"] = "acme-inc/guildnet-chat"

	if _, err := run(t, c, "github_open_pull_request", in); err != nil {
		t.Fatal(err)
	}
	if len(f.pulls("guildnet-chat")) != 1 {
		t.Error("expected a PR on guildnet-chat")
	}
}

func TestCommentingOnAPullRequest(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))

	_, err := run(t, c, "github_comment", map[string]any{"repo": "guildnet", "number": 1.0, "body": "Tests pass locally."})

	if err != nil {
		t.Fatal(err)
	}
	if len(f.comment) != 1 || !strings.Contains(f.comment[0], "guildnet#1: Tests pass locally.") {
		t.Fatalf("comments = %q", f.comment)
	}
	// Signed like the agent's pull requests: who wrote it, and that it's an
	// AI agent.
	if !strings.Contains(f.comment[0], "\n---\nPosted by kai") || !strings.Contains(f.comment[0], "an AI agent, via GuildNet.") || strings.ContainsRune(f.comment[0], 0x2014) {
		t.Errorf("the comment is signed %q", f.comment[0])
	}
}

func TestStatusReportsChecksAndWhetherItCanMerge(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	f.setChecks("guildnet", "kai/healthz",
		fakeCheck{Name: "test", Status: "completed", Conclusion: "success"},
		fakeCheck{Name: "lint", Status: "completed", Conclusion: "failure"})

	out, err := run(t, c, "github_pr_status", map[string]any{"repo": "guildnet", "number": 1.0})

	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Add a health check", "open", "test: success", "lint: failure"} {
		if !strings.Contains(out, want) {
			t.Errorf("status %q should mention %q", out, want)
		}
	}
}

func TestMergingNeedsApprovalAndTheApproverSeesWhatTheyreApproving(t *testing.T) {
	c, _ := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	f := findTool(t, c, "github_merge_pull_request")
	if !f.Ask {
		t.Fatal("merging must pause for approval")
	}
	for _, name := range []string{"github_open_pull_request", "github_comment", "github_pr_status"} {
		if findTool(t, c, name).Ask {
			t.Errorf("%s shouldn't need approval", name)
		}
	}

	summary, _, err := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"merge", "#1", "Add a health check", "main", "guildnet"} {
		if !strings.Contains(summary, want) {
			t.Errorf("approval summary %q should mention %q", summary, want)
		}
	}
}

// The title is the agent's: however long it is, the facts the approver
// decides on (where it merges, how big it is, its checks) come first, and
// the title is cut short, so a message cut at 200 characters still has them.
func TestALongPRTitleDoesntPushTheChecksOutOfTheApproval(t *testing.T) {
	c, _ := newGitHub(t)
	pr := openPR("kai/healthz", file("health.go", "x"))
	pr["title"] = "Add a health check " + strings.Repeat("and something more ", 30)
	run(t, c, "github_open_pull_request", pr)

	summary, _, err := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if err != nil {
		t.Fatal(err)
	}
	first := []rune(summary)[:min(200, len([]rune(summary)))]
	for _, want := range []string{"#1", "into main", "guildnet", "1 files", "checks: ", "Add a health check"} {
		if !strings.Contains(string(first), want) {
			t.Errorf("the first 200 characters of %q should have %q", summary, want)
		}
	}
	if strings.Count(summary, "something more") > 4 {
		t.Errorf("the title isn't cut short: %q", summary)
	}
}

func TestAnApprovedMergeMergesWhatWasApproved(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	_, pinned, err := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})
	if err != nil {
		t.Fatal(err)
	}

	out, err := run(t, c, "github_merge_pull_request", pinned)

	if err != nil {
		t.Fatal(err)
	}
	if len(f.merges) != 1 || f.merges[0] != "guildnet#1 squash" {
		t.Errorf("merges = %q, want PR #1 squashed", f.merges)
	}
	if got, _ := f.file("guildnet", "main", "health.go"); got != "x" {
		t.Errorf("main should have the change after merging, health.go = %q", got)
	}
	if !strings.Contains(out, "Merged") {
		t.Errorf("the agent should be told it merged, got %q", out)
	}
}

func TestAMergeIsRefusedIfThePullRequestChangedAfterApproval(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	_, pinned, _ := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})
	f.pushAsSomeoneElse("guildnet", "kai/healthz")

	_, err := run(t, c, "github_merge_pull_request", pinned)

	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("err = %v, want a refusal because the PR changed since it was approved", err)
	}
	if len(f.merges) != 0 {
		t.Errorf("merged anyway: %q", f.merges)
	}
}

// The approver approved merging into one branch: a PR moved to another
// since (its author may) isn't merged.
func TestAMergeIsRefusedIfThePullRequestWasRetargetedAfterApproval(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	_, pinned, _ := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})
	f.retarget("guildnet", 1, "release")

	_, err := run(t, c, "github_merge_pull_request", pinned)

	if err == nil || !strings.Contains(err.Error(), "release") {
		t.Errorf("err = %v, want a refusal naming the branch it moved to", err)
	}
	if len(f.merges) != 0 {
		t.Errorf("merged anyway: %q", f.merges)
	}
}

func TestAMergeThatWasNeverApprovedIsRefused(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))

	_, err := run(t, c, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if err == nil {
		t.Error("a merge without an approval's pinned commit should be refused")
	}
	if len(f.merges) != 0 {
		t.Errorf("merged without approval: %q", f.merges)
	}
}

func TestPreparingAMergeOfAClosedPullRequestTellsTheAgent(t *testing.T) {
	c, _ := newGitHub(t)

	_, _, err := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 7.0})

	if err == nil {
		t.Error("there's no PR #7; the agent should be told before anyone is asked to approve")
	}
}

func TestTheWorkspaceGetsAReadOnlyTokenForJustTheCompanysRepos(t *testing.T) {
	c, f := newGitHub(t)

	_, token, err := c.Checkout(context.Background(), kai)

	if err != nil {
		t.Fatal(err)
	}
	g := f.lastGrant()
	if g.Token != token {
		t.Fatalf("token %q isn't the one minted last", token)
	}
	if len(g.Permissions) != 2 || g.Permissions["contents"] != "read" || g.Permissions["metadata"] != "read" {
		t.Errorf("sandbox token permissions = %v, want contents and metadata read-only", g.Permissions)
	}
	if strings.Join(g.Repositories, ",") != "guildnet,guildnet-chat" {
		t.Errorf("sandbox token repos = %v", g.Repositories)
	}

}

func TestTheWorkspaceTokenIsReusedUntilItNearsExpiry(t *testing.T) {
	c, f := newGitHub(t)
	_, first, _ := c.Checkout(context.Background(), kai)
	before := f.grantCount()

	_, second, _ := c.Checkout(context.Background(), kai)

	if first != second || f.grantCount() != before {
		t.Error("a fresh token was minted although the first has most of an hour left")
	}
}

func TestTheWorkspaceCanClonePrivateRepos(t *testing.T) {
	c, _ := newGitHub(t)

	urls, _, _ := c.Checkout(context.Background(), kai)

	want := "https://github.com/acme-inc/guildnet,https://github.com/acme-inc/guildnet-chat"
	if strings.Join(urls, ",") != want {
		t.Errorf("repo URLs = %v, want %s", urls, want)
	}
}

func TestTheGuideTellsTheAgentHowToChangeCode(t *testing.T) {
	c, _ := newGitHub(t)

	guide := c.Guide(kai)

	for _, want := range []string{"read-only", "github_open_pull_request", "edits", "patch", "git rebase origin/<base>", "...HEAD", "github_close_pull_request", "when a tool result says so", "/workspace/guildnet", "approv"} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide should mention %q:\n%s", want, guide)
		}
	}
}

func findTool(t *testing.T, c *github.Client, name string) (found struct{ Ask bool }) {
	t.Helper()
	for _, tool := range c.Tools(kai) {
		if tool.Name == name {
			if tool.Description == "" || len(tool.Properties) == 0 {
				t.Errorf("%s needs a description and an input schema", name)
			}
			found.Ask = tool.Ask
			return found
		}
	}
	t.Fatalf("no tool %s", name)
	return found
}

func TestStepsSayWhatTheAgentIsDoingOnGitHub(t *testing.T) {
	c, _ := newGitHub(t)

	cases := map[string]map[string]any{
		"opens a pull request on guildnet: Add a health check": {"repo": "guildnet", "title": "Add a health check", "branch": "kai/healthz"},
		"comments on guildnet #4":                              {"repo": "guildnet", "number": 4.0, "body": "hi"},
		"checks guildnet PR #4":                                {"repo": "guildnet", "number": 4.0},
		"merges guildnet PR #4":                                {"repo": "guildnet", "number": 4.0},
	}
	tools := map[string]string{
		"opens a pull request on guildnet: Add a health check": "github_open_pull_request",
		"comments on guildnet #4":                              "github_comment",
		"checks guildnet PR #4":                                "github_pr_status",
		"merges guildnet PR #4":                                "github_merge_pull_request",
	}
	for want, in := range cases {
		if got := c.Step(tools[want], in); got != want {
			t.Errorf("step for %s = %q, want %q", tools[want], got, want)
		}
	}
}

func TestWhenChecksCantBeReadTheApproverIsToldSoRatherThanMisled(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	f.denyChecks()

	summary, _, err := c.Prepare(context.Background(), kai, "github_merge_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})
	status, _ := run(t, c, "github_pr_status", map[string]any{"repo": "guildnet", "number": 1.0})

	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "checks: unknown") || strings.Contains(summary, "running") || strings.Contains(summary, "passed") {
		t.Errorf("summary %q should say the checks are unknown", summary)
	}
	if !strings.Contains(status, "can't read checks") {
		t.Errorf("status %q should say checks can't be read", status)
	}
}

func edit(path string, pairs ...string) map[string]any {
	var edits []any
	for i := 0; i+1 < len(pairs); i += 2 {
		edits = append(edits, map[string]any{"old": pairs[i], "new": pairs[i+1]})
	}
	return map[string]any{"path": path, "edits": edits}
}

func TestAnAgentCanSendEditsInsteadOfWholeFiles(t *testing.T) {
	c, f := newGitHub(t)

	_, err := run(t, c, "github_open_pull_request", openPR("kai/readme", edit("README.md", "# guildnet", "# GuildNet\n\nAn IRC network for companies.")))

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.file("guildnet", "kai/readme", "README.md"); got != "# GuildNet\n\nAn IRC network for companies.\n" {
		t.Errorf("README.md = %q, want only the edited text changed", got)
	}
}

func TestEditsBuildOnTheBranchNotOnMain(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/code", file("main.go", "package main\n\nfunc main() {}\n")))

	_, err := run(t, c, "github_open_pull_request", openPR("kai/code", edit("main.go", "func main() {}", "func main() { run() }")))

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.file("guildnet", "kai/code", "main.go"); got != "package main\n\nfunc main() { run() }\n" {
		t.Errorf("main.go = %q", got)
	}
}

func TestAnEditThatDoesntMatchIsRefusedAndNothingIsCommitted(t *testing.T) {
	c, f := newGitHub(t)

	_, err := run(t, c, "github_open_pull_request", openPR("kai/x", edit("README.md", "no such text", "y")))

	if err == nil || !strings.Contains(err.Error(), "README.md") {
		t.Errorf("err = %v, want one naming the file whose edit didn't match", err)
	}
	if len(f.pulls("guildnet")) != 0 {
		t.Error("nothing should be committed or opened")
	}
}

func TestAnEditThatMatchesInSeveralPlacesIsRefused(t *testing.T) {
	c, _ := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/x", file("a.txt", "one\none\n")))

	_, err := run(t, c, "github_open_pull_request", openPR("kai/x", edit("a.txt", "one", "two")))

	if err == nil || !strings.Contains(err.Error(), "2 places") {
		t.Errorf("err = %v, want a refusal asking for more context", err)
	}
}

func TestEditingAFileThatDoesntExistIsRefused(t *testing.T) {
	c, _ := newGitHub(t)

	_, err := run(t, c, "github_open_pull_request", openPR("kai/x", edit("nope.go", "a", "b")))

	if err == nil || !strings.Contains(err.Error(), "nope.go") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdatingABranchBringsInWhatsNewOnItsBase(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	f.pushToMain("guildnet", "other.go", "someone else's change")

	out, err := run(t, c, "github_update_branch", map[string]any{"repo": "guildnet", "number": 1.0})

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.file("guildnet", "kai/healthz", "other.go"); got != "someone else's change" {
		t.Error("the branch should now have main's new file")
	}
	if got, _ := f.file("guildnet", "kai/healthz", "health.go"); got != "x" {
		t.Error("the branch should keep its own change")
	}
	if !strings.Contains(out, "Updated PR #1") {
		t.Errorf("out = %q", out)
	}
}

func TestAConflictingUpdateTellsTheAgentWhatToDoInstead(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/readme", file("README.md", "kai's version")))
	f.pushToMain("guildnet", "README.md", "someone else's version")

	_, err := run(t, c, "github_update_branch", map[string]any{"repo": "guildnet", "number": 1.0})

	if err == nil || !strings.Contains(err.Error(), "conflicts") || !strings.Contains(err.Error(), "new branch") {
		t.Errorf("err = %v, want the conflict explained with what to do instead", err)
	}
}

// Each team gets its own repos.

var (
	engineer = config.Persona{Nick: "kai", Name: "Kai", Team: "Engineering"}
	marketer = config.Persona{Nick: "pax", Name: "Pax", Team: "Marketing"}
	product  = config.Persona{Nick: "vera", Name: "Vera", Team: "Product"}
)

func newGitHubWithTeams(t *testing.T) (*github.Client, *fakeGitHub) {
	t.Helper()
	f := newFakeGitHub(t, "guildnet", "guildnet-chat", "guildnet-site")
	c, err := github.New(github.Options{AppID: 1, InstallationID: 2, Key: testKey(t), Owner: owner,
		Repos:   []string{"guildnet", "guildnet-chat", "guildnet-site"},
		Teams:   map[string][]string{"Engineering": {"guildnet", "guildnet-chat"}, "marketing": {"guildnet-site"}},
		BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestEachTeamWorksOnlyOnItsOwnRepos(t *testing.T) {
	c, f := newGitHubWithTeams(t)
	site := openPR("pax/copy", file("index.md", "# GuildNet"))
	site["repo"] = "guildnet-site"

	if _, err := c.Run(context.Background(), marketer, "github_open_pull_request", site); err != nil {
		t.Errorf("Marketing should be able to use guildnet-site: %v", err)
	}
	if _, err := c.Run(context.Background(), marketer, "github_open_pull_request", openPR("pax/x", file("a", "a"))); err == nil {
		t.Error("Marketing shouldn't be able to touch guildnet")
	}
	site["branch"] = "kai/copy"
	if _, err := c.Run(context.Background(), engineer, "github_open_pull_request", site); err == nil {
		t.Error("Engineering shouldn't be able to touch guildnet-site")
	}
	if len(f.pulls("guildnet-site")) != 1 || len(f.pulls("guildnet")) != 0 {
		t.Errorf("pulls: site %d, guildnet %d", len(f.pulls("guildnet-site")), len(f.pulls("guildnet")))
	}
}

func TestAnAgentIsOfferedOnlyItsTeamsRepos(t *testing.T) {
	c, _ := newGitHubWithTeams(t)

	tools := c.Tools(marketer)

	if len(tools) == 0 {
		t.Fatal("Marketing should get the GitHub tools")
	}
	enum := tools[0].Properties["repo"].(map[string]any)["enum"]
	if fmt.Sprint(enum) != "[guildnet-site]" {
		t.Errorf("repos offered to Marketing = %v", enum)
	}
	if guide := c.Guide(marketer); !strings.Contains(guide, "/workspace/guildnet-site") || strings.Contains(guide, "/workspace/guildnet,") {
		t.Errorf("guide should name only Marketing's repo:\n%s", guide)
	}
}

func TestATeamWithoutReposGetsNoGitHub(t *testing.T) {
	c, f := newGitHubWithTeams(t)

	urls, token, err := c.Checkout(context.Background(), product)

	if err != nil || len(urls) != 0 || token != "" {
		t.Errorf("checkout for Product = %v %q %v, want nothing", urls, token, err)
	}
	if f.grantCount() != 0 {
		t.Error("no token should be minted for a team without repos")
	}
	if len(c.Tools(product)) != 0 || c.Guide(product) != "" {
		t.Error("Product should get no GitHub tools or guide")
	}
}

func TestAWorkspaceTokenCoversOnlyItsTeamsRepos(t *testing.T) {
	c, f := newGitHubWithTeams(t)

	urls, _, err := c.Checkout(context.Background(), marketer)

	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(urls, ",") != "https://github.com/acme-inc/guildnet-site" {
		t.Errorf("urls = %v", urls)
	}
	if g := f.lastGrant(); strings.Join(g.Repositories, ",") != "guildnet-site" {
		t.Errorf("Marketing's workspace token covers %v, want only guildnet-site", g.Repositories)
	}
}

func TestAgentsCommitOnlyToTheirOwnBranches(t *testing.T) {
	c, f := newGitHub(t)

	for _, branch := range []string{"pax/copy", "claude/ledger-detail", "healthz"} {
		_, err := run(t, c, "github_open_pull_request", openPR(branch, file("a.txt", "a")))
		if err == nil || !strings.Contains(err.Error(), "kai/") {
			t.Errorf("branch %s: err = %v, want a refusal pointing at kai/<topic>", branch, err)
		}
	}
	if len(f.pulls("guildnet")) != 0 {
		t.Error("no PR should have been opened")
	}
}

func TestAnAgentCantUpdateSomeoneElsesBranch(t *testing.T) {
	c, f := newGitHub(t)
	pax := config.Persona{Nick: "pax", Name: "Pax"}
	c.Run(context.Background(), pax, "github_open_pull_request", openPR("pax/copy", file("a.txt", "a")))
	f.pushToMain("guildnet", "b.txt", "b")

	_, err := run(t, c, "github_update_branch", map[string]any{"repo": "guildnet", "number": 1.0})

	if err == nil || !strings.Contains(err.Error(), "pax/copy") {
		t.Errorf("err = %v, want a refusal to touch pax's branch", err)
	}
}

// A patch, as `git diff main` prints it: edit README.md, add new.go, delete main.go.
const workspacePatch = `diff --git a/README.md b/README.md
index 1111111..2222222 100644
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-# guildnet
+# GuildNet
diff --git a/new.go b/new.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+package main
diff --git a/main.go b/main.go
deleted file mode 100644
index 4444444..0000000
--- a/main.go
+++ /dev/null
@@ -1 +0,0 @@
-package main
`

func TestAnAgentCanSendTheCommitsItMadeInItsWorkspaceAsAPatch(t *testing.T) {
	c, f := newGitHub(t)
	in := openPR("kai/rename")
	delete(in, "files")
	in["patch"] = workspacePatch

	_, err := run(t, c, "github_open_pull_request", in)

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.file("guildnet", "kai/rename", "README.md"); got != "# GuildNet\n" {
		t.Errorf("README.md = %q", got)
	}
	if got, ok := f.file("guildnet", "kai/rename", "new.go"); !ok || got != "package main\n" {
		t.Errorf("new.go = %q, %v", got, ok)
	}
	if _, ok := f.file("guildnet", "kai/rename", "main.go"); ok {
		t.Error("main.go should be deleted")
	}
	if len(f.pulls("guildnet")) != 1 {
		t.Error("expected a PR")
	}
}

func TestAPatchThatDoesntFitTheBranchIsRefused(t *testing.T) {
	c, f := newGitHub(t)
	f.pushToMain("guildnet", "README.md", "# Something else\n")
	in := openPR("kai/rename")
	delete(in, "files")
	in["patch"] = workspacePatch

	_, err := run(t, c, "github_open_pull_request", in)

	if err == nil || !strings.Contains(err.Error(), "README.md") {
		t.Errorf("err = %v, want a refusal naming the file that didn't apply", err)
	}
	if len(f.pulls("guildnet")) != 0 {
		t.Error("nothing should be opened when the patch doesn't apply")
	}
}

func TestWithoutFilesOrAPatchTheAgentIsToldHowToSendItsCommits(t *testing.T) {
	c, _ := newGitHub(t)
	in := openPR("kai/x")
	delete(in, "files")

	_, err := run(t, c, "github_open_pull_request", in)

	if err == nil || !strings.Contains(err.Error(), "git diff") {
		t.Errorf("err = %v, want a hint to send a patch from git diff", err)
	}
}

func TestAnAgentCanCloseItsOwnPullRequest(t *testing.T) {
	c, f := newGitHub(t)
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))

	out, err := run(t, c, "github_close_pull_request", map[string]any{"repo": "guildnet", "number": 1.0, "comment": "Superseded by #2."})

	if err != nil {
		t.Fatal(err)
	}
	if p := f.pulls("guildnet")[0]; p.State != "closed" || p.Merged {
		t.Errorf("PR = %+v, want closed and not merged", p)
	}
	if len(f.comment) != 1 || !strings.Contains(f.comment[0], "Superseded by #2.") {
		t.Errorf("comments = %q, want the reason left on the PR", f.comment)
	}
	if !strings.Contains(out, "Closed PR #1") {
		t.Errorf("out = %q", out)
	}
}

func TestAnAgentCantCloseSomeoneElsesPullRequest(t *testing.T) {
	c, f := newGitHub(t)
	pax := config.Persona{Nick: "pax", Name: "Pax", Role: "Copywriter"}
	c.Run(context.Background(), pax, "github_open_pull_request", openPR("pax/copy", file("a.txt", "a")))

	_, err := run(t, c, "github_close_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if err == nil {
		t.Error("kai shouldn't be able to close pax's PR")
	}
	if f.pulls("guildnet")[0].State != "open" {
		t.Error("pax's PR was closed")
	}
}

// What makes a PR an agent's is what nobody can write into one: the App's
// bot opened it, from the repo itself. A person's PR on a branch named for
// the agent, or with the agent's footer, isn't the agent's to close.
func TestAnAgentCantCloseAPersonsPullRequestThatLooksLikeItsOwn(t *testing.T) {
	c, f := newGitHub(t)
	f.personOpens("guildnet", "mallory", "kai/fix", "Opened by kai (Backend engineer), an AI agent, via GuildNet.", false)

	_, err := run(t, c, "github_close_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if err == nil || f.pulls("guildnet")[0].State != "open" {
		t.Errorf("kai closed mallory's PR: %v", err)
	}
}

// Not knowing its App's bot (a runner given the App's key, say), an agent
// may still close a bot's PR on its own branch, from the repo itself, and
// nobody else's.
func TestNotKnowingItsBotAnAgentClosesOnlyABotsPullRequestOnItsBranch(t *testing.T) {
	f := newFakeGitHub(t, "guildnet")
	c, err := github.New(github.Options{AppID: 1, InstallationID: 2, Key: testKey(t), Owner: owner, Repos: []string{"guildnet"}, BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	run(t, c, "github_open_pull_request", openPR("kai/healthz", file("health.go", "x")))
	f.personOpens("guildnet", "mallory", "kai/fix", "", false)

	_, theirs := run(t, c, "github_close_pull_request", map[string]any{"repo": "guildnet", "number": 2.0})
	_, own := run(t, c, "github_close_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if theirs == nil {
		t.Error("kai closed mallory's PR on a kai/ branch")
	}
	if own != nil {
		t.Errorf("kai's own PR: %v", own)
	}
}

func TestAnAgentCanCloseAPullRequestItOpenedBeforeBranchNamesWereEnforced(t *testing.T) {
	c, f := newGitHub(t)
	// Kai's PR #4 was on claude/ledger-detail, opened by kai through the tool.
	run(t, c, "github_open_pull_request", openPR("kai/tmp", file("a.txt", "a")))
	f.renameHead("guildnet", 1, "claude/ledger-detail")

	_, err := run(t, c, "github_close_pull_request", map[string]any{"repo": "guildnet", "number": 1.0})

	if err != nil {
		t.Errorf("kai opened it, so kai can close it: %v", err)
	}
}

func TestARenameInAPatchMovesTheFile(t *testing.T) {
	c, f := newGitHub(t)
	in := openPR("kai/move")
	delete(in, "files")
	in["patch"] = "diff --git a/main.go b/cmd.go\nsimilarity index 100%\nrename from main.go\nrename to cmd.go\n"

	if _, err := run(t, c, "github_open_pull_request", in); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.file("guildnet", "kai/move", "cmd.go"); !ok || got != "package main\n" {
		t.Errorf("cmd.go = %q, %v", got, ok)
	}
	if _, ok := f.file("guildnet", "kai/move", "main.go"); ok {
		t.Error("main.go should be gone after the rename")
	}
}
