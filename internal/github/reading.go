// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	gh "github.com/google/go-github/v92/github"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Agents without a workspace (chat agents, on any model) can't read code in
// a checkout, so they get tools that read it from GitHub: a file, the files
// under a folder, a code search. They change code as agents at desks do,
// through github_open_pull_request, with edits.

// How much one read gives back, so a big file doesn't swamp the model.
const (
	maxReadLines = 1000
	maxReadBytes = 48 << 10
	maxListed    = 500
	maxFound     = 30
)

// WithoutWorkspace is the toolbox for agents without a workspace: the read
// tools, and the rest of GuildNet's GitHub tools, for the same repos.
func (c *Client) WithoutWorkspace() agent.Toolbox { return remote{c} }

type remote struct{ c *Client }

func (r remote) Tools(p config.Persona) []agent.Tool {
	tools := r.c.Tools(p)
	if len(tools) == 0 {
		return nil
	}
	repo := map[string]any{"type": "string", "enum": r.c.reposFor(p), "description": "Repository name"}
	ref := map[string]any{"type": "string", "description": "Branch, tag, or commit; defaults to the repo's default branch"}
	read := []agent.Tool{
		{
			Name:        "github_read_file",
			ReadOnly:    true,
			Description: fmt.Sprintf("Read a file from one of your team's repositories. Long files come in parts of up to %d lines: use start_line and end_line for the rest.", maxReadLines),
			Properties: map[string]any{
				"repo": repo, "ref": ref,
				"path":       map[string]any{"type": "string", "description": "Path from the repo root, e.g. cmd/runner/main.go"},
				"start_line": map[string]any{"type": "integer", "description": "First line to read (from 1)"},
				"end_line":   map[string]any{"type": "integer", "description": "Last line to read"},
			},
			Required: []string{"repo", "path"},
		},
		{
			Name:        "github_list_files",
			ReadOnly:    true,
			Description: "List the files in one of your team's repositories, or under a folder of it.",
			Properties: map[string]any{
				"repo": repo, "ref": ref,
				"path": map[string]any{"type": "string", "description": "A folder, e.g. internal/runner; the whole repo if left out"},
			},
			Required: []string{"repo"},
		},
		{
			Name:        "github_search_code",
			ReadOnly:    true,
			Description: "Search the code on a repository's default branch (GitHub code search): the files that match, each with a matching line.",
			Properties: map[string]any{
				"repo":  repo,
				"query": map[string]any{"type": "string", "description": "Words or an identifier to find, e.g. healthz"},
			},
			Required: []string{"repo", "query"},
		},
	}
	return append(read, tools...)
}

func (r remote) Guide(p config.Persona) string {
	repos := r.c.reposFor(p)
	if len(repos) == 0 {
		return ""
	}
	var names []string
	for _, repo := range repos {
		names = append(names, r.c.owner()+"/"+repo)
	}
	return fmt.Sprintf(`<github>
Your team's repositories are %s. You have no workspace or shell: read code with github_list_files, github_read_file, and github_search_code. Look before you answer; don't guess about code you could read.

To change code, use github_open_pull_request with files: edits for existing files (the exact old text, copied from github_read_file, and its replacement) and content only for new ones. Put your change on a branch of your own, named <your nick>/<topic> (other names are refused). Calling it again with the same branch adds a commit to that pull request. You can't run tests, so say so, and let the PR's checks (github_pr_status) tell you whether it works. If a PR falls behind its base branch, github_update_branch brings it up to date. github_close_pull_request closes one of your PRs, with a comment saying why; github_comment replies on a PR or issue.

Only say something is opened, closed, or merged when a tool result says so. If a tool failed, say that plainly.

Merging (github_merge_pull_request) waits for a person's approval, and merges exactly what they approved. Ask for it when the PR is ready and its checks pass.
</github>`, strings.Join(names, ", "))
}

func (r remote) Prepare(ctx context.Context, p config.Persona, name string, in map[string]any) (string, map[string]any, error) {
	return r.c.Prepare(ctx, p, name, in)
}

func (r remote) Run(ctx context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	var read func(context.Context, config.Persona, map[string]any) (string, error)
	switch name {
	case "github_read_file":
		read = r.c.readFile
	case "github_list_files":
		read = r.c.listFiles
	case "github_search_code":
		read = r.c.searchCode
	default:
		return r.c.Run(ctx, p, name, in)
	}
	out, err := read(ctx, p, in)
	if err != nil {
		return "", err
	}
	// Anyone who can open a PR or an issue can write what's read.
	return agent.Untrusted("github", out), nil
}

// SourceOf says GitHub's tools read and write GitHub (agent.Sourced).
func (r remote) SourceOf(name string) string { return r.c.SourceOf(name) }

// Step describes a call for IRC.
func (r remote) Step(name string, in map[string]any) string {
	repo := str(in, "repo")
	switch name {
	case "github_read_file":
		return "reads " + repo + "/" + strings.TrimPrefix(str(in, "path"), "/")
	case "github_list_files":
		if dir := strings.Trim(str(in, "path"), "/"); dir != "" {
			return "lists files in " + repo + "/" + dir
		}
		return "lists files in " + repo
	case "github_search_code":
		return fmt.Sprintf("searches %s for %s", repo, str(in, "query"))
	}
	return r.c.Step(name, in)
}

// cleanPath is a path in a repo, or an error for one that climbs out.
func cleanPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" || p == "." {
		return "", nil
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("bad path %q", p)
	}
	return path.Clean(p), nil
}

func intIn(in map[string]any, k string) int {
	switch v := in[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (c *Client) readFile(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, err := c.repo(p, in)
	if err != nil {
		return "", err
	}
	file, err := cleanPath(str(in, "path"))
	if err != nil {
		return "", err
	}
	if file == "" {
		return "", errors.New("path is required: a file; github_list_files shows what's there")
	}
	ref := str(in, "ref")
	f, dir, resp, err := c.gh.Repositories.GetContents(ctx, c.owner(), repo, file, &gh.RepositoryContentGetOptions{Ref: ref})
	where := repo + "/" + file
	if ref != "" {
		where += " at " + ref
	}
	switch {
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%s doesn't exist; github_list_files shows what's there", where)
	case err != nil:
		return "", apiErr("reading "+file, err)
	case f == nil || dir != nil:
		return "", fmt.Errorf("%s is a folder; github_list_files shows what's in it", where)
	}
	content, err := f.GetContent()
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", where, err)
	}
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return where + " is empty.", nil
	}
	start := max(intIn(in, "start_line"), 1)
	if start > len(lines) {
		return "", fmt.Errorf("%s has %d lines", where, len(lines))
	}
	end := intIn(in, "end_line")
	if end < start || end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	shown := start - 1
	for i := start - 1; i < end && i-start+1 < maxReadLines && b.Len()+len(lines[i]) <= maxReadBytes; i++ {
		b.WriteString(lines[i])
		shown = i + 1
	}
	out := fmt.Sprintf("%s, lines %d-%d of %d:\n%s", where, start, shown, len(lines), b.String())
	if shown < end {
		out += fmt.Sprintf("\n[That's lines %d-%d of %d. For more, call again with start_line: %d.]", start, shown, len(lines), shown+1)
	}
	return out, nil
}

func (c *Client) listFiles(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, err := c.repo(p, in)
	if err != nil {
		return "", err
	}
	dir, err := cleanPath(str(in, "path"))
	if err != nil {
		return "", err
	}
	ref := str(in, "ref")
	if ref == "" {
		r, _, err := c.gh.Repositories.Get(ctx, c.owner(), repo)
		if err != nil {
			return "", apiErr("reading "+repo, err)
		}
		ref = r.GetDefaultBranch()
	}
	tree, resp, err := c.gh.Git.GetTree(ctx, c.owner(), repo, ref, true)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s has no branch or commit %s", repo, ref)
	}
	if err != nil {
		return "", apiErr("listing "+repo, err)
	}
	var files []string
	for _, e := range tree.Entries {
		if e.GetType() != "blob" {
			continue
		}
		if dir == "" || strings.HasPrefix(e.GetPath(), dir+"/") {
			files = append(files, e.GetPath())
		}
	}
	where := repo
	if dir != "" {
		where += "/" + dir
	}
	if len(files) == 0 {
		return fmt.Sprintf("No files in %s at %s.", where, ref), nil
	}
	out := fmt.Sprintf("%d files in %s at %s:\n", len(files), where, ref)
	if len(files) > maxListed {
		out += strings.Join(files[:maxListed], "\n") + fmt.Sprintf("\n[and %d more; list a folder to see them]", len(files)-maxListed)
	} else {
		out += strings.Join(files, "\n")
	}
	if tree.GetTruncated() {
		out += "\n[GitHub cut this list short; list a folder for the rest]"
	}
	return out, nil
}

func (c *Client) searchCode(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, err := c.repo(p, in)
	if err != nil {
		return "", err
	}
	query := str(in, "query")
	if query == "" {
		return "", errors.New("query is required")
	}
	res, _, err := c.gh.Search.Code(ctx, query+" repo:"+c.owner()+"/"+repo, &gh.SearchOptions{TextMatch: true, ListOptions: gh.ListOptions{PerPage: maxFound}})
	if err != nil {
		return "", apiErr("searching "+repo, err)
	}
	if len(res.CodeResults) == 0 {
		return fmt.Sprintf("Nothing in %s matches %q (code search covers the default branch).", repo, query), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d files in %s match %q:\n", res.GetTotal(), repo, query)
	for _, r := range res.CodeResults {
		line := ""
		if len(r.TextMatches) > 0 {
			line, _, _ = strings.Cut(strings.TrimSpace(r.TextMatches[0].GetFragment()), "\n")
		}
		fmt.Fprintf(&b, "%s: %s\n", r.GetPath(), strings.TrimSpace(line))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
