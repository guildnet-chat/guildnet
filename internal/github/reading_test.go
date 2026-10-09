// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Agents without a workspace (chat agents, on any model) read their team's
// code through tools instead: a file, the files in a folder, a code search.
// They get GuildNet's other GitHub tools too, and change code with edits.

func remote(t *testing.T) (agent.Toolbox, *fakeGitHub) {
	t.Helper()
	c, f := newGitHub(t)
	return c.WithoutWorkspace(), f
}

func use(t *testing.T, tb agent.Toolbox, tool string, input map[string]any) (string, error) {
	t.Helper()
	return tb.Run(context.Background(), kai, tool, input)
}

func toolNames(tb agent.Toolbox) []string {
	var names []string
	for _, tool := range tb.Tools(kai) {
		names = append(names, tool.Name)
	}
	return names
}

func TestAnAgentWithoutAWorkspaceGetsToolsToReadCode(t *testing.T) {
	tb, _ := remote(t)
	names := toolNames(tb)
	for _, want := range []string{"github_read_file", "github_list_files", "github_search_code", "github_open_pull_request", "github_merge_pull_request"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v should include %s", names, want)
		}
	}
	c, _ := newGitHub(t)
	if desk := toolNamesOf(c.Tools(kai)); slices.Contains(desk, "github_read_file") {
		t.Error("an agent at a desk reads code in its workspace; it doesn't need read tools")
	}
}

func toolNamesOf(tools []agent.Tool) []string {
	var out []string
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

func TestReadingAFile(t *testing.T) {
	tb, _ := remote(t)

	out, err := use(t, tb, "github_read_file", map[string]any{"repo": "guildnet", "path": "README.md"})

	if err != nil || !strings.Contains(out, "# guildnet") || !strings.Contains(out, "README.md") {
		t.Errorf("reading README.md: %q, %v", out, err)
	}
}

func TestReadingAFileOnABranch(t *testing.T) {
	tb, _ := remote(t)
	use(t, tb, "github_open_pull_request", openPR("kai/healthz", file("health.go", "package main\n// healthz\n")))

	onBranch, err := use(t, tb, "github_read_file", map[string]any{"repo": "guildnet", "path": "health.go", "ref": "kai/healthz"})
	if err != nil || !strings.Contains(onBranch, "// healthz") {
		t.Errorf("on the branch: %q, %v", onBranch, err)
	}
	if _, err := use(t, tb, "github_read_file", map[string]any{"repo": "guildnet", "path": "health.go"}); err == nil {
		t.Error("health.go isn't on main yet")
	}
}

func TestALongFileIsReadInParts(t *testing.T) {
	tb, f := remote(t)
	var long strings.Builder
	for i := 1; i <= 3000; i++ {
		long.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	f.pushToMain("guildnet", "big.txt", long.String())

	first, _ := use(t, tb, "github_read_file", map[string]any{"repo": "guildnet", "path": "big.txt"})
	if !strings.Contains(first, "line 1\n") || strings.Contains(first, "line 3000") || !strings.Contains(first, "start_line") {
		t.Errorf("the first part should end with how to read on; got %d bytes ending %q", len(first), tail(first))
	}
	later, _ := use(t, tb, "github_read_file", map[string]any{"repo": "guildnet", "path": "big.txt", "start_line": 2990, "end_line": 3000})
	if !strings.Contains(later, "line 2990\n") || !strings.Contains(later, "line 3000") || strings.Contains(later, "line 2989\n") {
		t.Errorf("lines 2990-3000: %q", later)
	}
}

func TestListingFiles(t *testing.T) {
	tb, f := remote(t)
	f.pushToMain("guildnet", "cmd/runner/main.go", "package main\n")

	all, err := use(t, tb, "github_list_files", map[string]any{"repo": "guildnet"})
	if err != nil || !strings.Contains(all, "README.md") || !strings.Contains(all, "cmd/runner/main.go") {
		t.Errorf("all files: %q, %v", all, err)
	}
	under, _ := use(t, tb, "github_list_files", map[string]any{"repo": "guildnet", "path": "cmd"})
	if !strings.Contains(under, "cmd/runner/main.go") || strings.Contains(under, "README.md") {
		t.Errorf("files under cmd: %q", under)
	}
}

func TestSearchingCode(t *testing.T) {
	tb, f := remote(t)
	f.pushToMain("guildnet", "health.go", "package main\n\nfunc healthz() {}\n")

	out, err := use(t, tb, "github_search_code", map[string]any{"repo": "guildnet", "query": "healthz"})

	if err != nil || !strings.Contains(out, "health.go") || strings.Contains(out, "README.md") {
		t.Errorf("searching for healthz: %q, %v", out, err)
	}
	if got := f.lastSearch(); !strings.Contains(got, "repo:"+owner+"/guildnet") {
		t.Errorf("the search %q should be limited to the repo", got)
	}
}

func TestReadingIsLimitedToTheTeamsRepos(t *testing.T) {
	tb, _ := remote(t)
	for _, tool := range []string{"github_read_file", "github_list_files", "github_search_code"} {
		if _, err := use(t, tb, tool, map[string]any{"repo": "secret-plans", "path": "README.md", "query": "x"}); err == nil {
			t.Errorf("%s read secret-plans, which isn't one of the network's repos", tool)
		}
	}
}

// Without a workspace, the guide says how to read code and change it with
// edits, not about a checkout.
func TestTheGuideForAnAgentWithoutAWorkspace(t *testing.T) {
	tb, _ := remote(t)
	g := tb.Guide(kai)
	if !strings.Contains(g, "github_read_file") || !strings.Contains(g, "edits") || strings.Contains(g, "/workspace/") {
		t.Errorf("guide:\n%s", g)
	}
}

func TestStepsSayWhatAnAgentReads(t *testing.T) {
	tb, _ := remote(t)
	s := tb.(interface {
		Step(name string, input map[string]any) string
	})
	for _, c := range []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"github_read_file", map[string]any{"repo": "guildnet", "path": "README.md"}, "reads guildnet/README.md"},
		{"github_list_files", map[string]any{"repo": "guildnet"}, "lists files in guildnet"},
		{"github_list_files", map[string]any{"repo": "guildnet", "path": "cmd"}, "lists files in guildnet/cmd"},
		{"github_search_code", map[string]any{"repo": "guildnet", "query": "healthz"}, "searches guildnet for healthz"},
		{"github_pr_status", map[string]any{"repo": "guildnet", "number": float64(3)}, "checks guildnet PR #3"},
	} {
		if got := s.Step(c.tool, c.input); got != c.want {
			t.Errorf("%s: %q, want %q", c.tool, got, c.want)
		}
	}
}

// A narrower toolbox is still the same client: approvals work as before.
func TestMergingStillNeedsApprovalWithoutAWorkspace(t *testing.T) {
	tb, _ := remote(t)
	for _, tool := range tb.Tools(kai) {
		if tool.Name == "github_merge_pull_request" && !tool.Ask {
			t.Error("merging must wait for a person")
		}
		if strings.HasPrefix(tool.Name, "github_read") && tool.Ask {
			t.Error("reading needs no approval")
		}
	}
}

func tail(s string) string {
	if len(s) > 200 {
		return s[len(s)-200:]
	}
	return s
}
