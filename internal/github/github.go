// SPDX-License-Identifier: AGPL-3.0-only

// Package github is GuildNet's GitHub integration, as a GitHub App. Agents
// read code in their workspace with a short-lived read-only token, and
// change it only through the tools here, which the runner carries out with
// the app's own credentials: opening pull requests, commenting, checking
// status, and (after a person approves) merging.
package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v92/github"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Options configure the client.
type Options struct {
	// Tokens is where installation tokens come from. Without it, they're
	// minted with the app's own key (AppID, InstallationID, Key): only for
	// our own network, since the key can mint tokens for every installation.
	Tokens Tokens
	// Live, when set, is the installation's account and repos as they are
	// now, in place of Owner and Repos: a customer's, which change while the
	// runner runs.
	Live           *Live
	AppID          int64
	InstallationID int64
	Key            []byte   // the app's private key, PEM
	Owner          string   // the account that owns the repos, e.g. "acme-inc"
	Repos          []string // the only repos agents can use, e.g. "guildnet"
	// Teams limits each team to some of Repos, by team name. Agents in a
	// team that isn't listed get no GitHub. Without Teams, every agent gets
	// every repo.
	Teams map[string][]string
	// TeamRepos, when set, is which repos a team works on, as the org says;
	// it takes the place of Teams. Only repos in Repos are ever given.
	TeamRepos func(team string) []string
	BaseURL   string // the API; empty means api.github.com (tests use a fake)
}

// Client carries out GitHub actions for agents. It implements agent.Toolbox.
type Client struct {
	opts   Options
	tokens Tokens
	gh     *gh.Client // with a token for the network's repos and what the tools need

	mu      sync.Mutex
	cache   map[string]string    // repos and mode -> the current token
	expires map[string]time.Time // and when it expires
}

// Tokens mints installation tokens for some of an installation's repos:
// read-only (for workspaces), or with what the tools need (write).
type Tokens interface {
	Token(ctx context.Context, repos []string, write bool) (token string, expires time.Time, err error)
}

// What tokens may do. The tools' tokens may change contents, pull requests,
// and issues, and read checks and statuses; never workflows, so nothing an
// agent does can change a repo's CI.
var (
	readOnly  = &gh.InstallationPermissions{Contents: gh.Ptr("read"), Metadata: gh.Ptr("read")}
	toolsNeed = &gh.InstallationPermissions{Contents: gh.Ptr("write"), PullRequests: gh.Ptr("write"), Issues: gh.Ptr("write"),
		Checks: gh.Ptr("read"), Statuses: gh.Ptr("read"), Metadata: gh.Ptr("read")}
)

// App is a GitHub App, minting tokens for its installations with its key.
type App struct {
	apps *gh.Client

	mu      sync.Mutex
	granted map[int64]grant // what each installation was granted, lately
}

type grant struct {
	perms *gh.InstallationPermissions
	at    time.Time
}

// NewApp is the app with this id and private key (PEM); baseURL is the API,
// empty for api.github.com.
func NewApp(appID int64, key []byte, baseURL string) (*App, error) {
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, key)
	if err != nil {
		return nil, fmt.Errorf("github: reading the app's private key: %w", err)
	}
	if baseURL != "" {
		atr.BaseURL = strings.TrimRight(baseURL, "/")
	}
	c, err := apiClient(atr, baseURL)
	if err != nil {
		return nil, err
	}
	return &App{apps: c, granted: map[int64]grant{}}, nil
}

// Token mints a token for installation, limited to repos, read-only or with
// what the tools need.
func (a *App) Token(ctx context.Context, installation int64, repos []string, write bool) (string, time.Time, error) {
	perms := readOnly
	if write {
		var err error
		if perms, err = a.toolsPermissions(ctx, installation); err != nil {
			return "", time.Time{}, err
		}
	}
	t, _, err := a.apps.Apps.CreateInstallationToken(ctx, installation, &gh.InstallationTokenOptions{Repositories: repos, Permissions: perms})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github: minting a token: %w", err)
	}
	return t.GetToken(), t.GetExpiresAt().Time, nil
}

// toolsPermissions is what the tools need, less the optional reads (checks,
// commit statuses) the installation wasn't granted: asking for a permission
// it doesn't have fails the whole token.
func (a *App) toolsPermissions(ctx context.Context, installation int64) (*gh.InstallationPermissions, error) {
	a.mu.Lock()
	g, ok := a.granted[installation]
	a.mu.Unlock()
	if !ok || time.Since(g.at) > 10*time.Minute {
		inst, _, err := a.apps.Apps.GetInstallation(ctx, installation)
		if err != nil {
			return nil, fmt.Errorf("github: reading what the installation may do: %w", err)
		}
		g = grant{perms: inst.GetPermissions(), at: time.Now()}
		a.mu.Lock()
		a.granted[installation] = g
		a.mu.Unlock()
	}
	for what, have := range map[string]string{"contents": g.perms.GetContents(), "pull requests": g.perms.GetPullRequests(), "issues": g.perms.GetIssues()} {
		if have != "write" {
			return nil, fmt.Errorf("github: the app may not change %s in this installation, and the tools need to", what)
		}
	}
	p := *toolsNeed
	if g.perms.GetChecks() == "" {
		p.Checks = nil
	}
	if g.perms.GetStatuses() == "" {
		p.Statuses = nil
	}
	return &p, nil
}

// For is the app's tokens for one installation.
func (a *App) For(installation int64) Tokens { return installationTokens{a, installation} }

type installationTokens struct {
	app          *App
	installation int64
}

func (i installationTokens) Token(ctx context.Context, repos []string, write bool) (string, time.Time, error) {
	return i.app.Token(ctx, i.installation, repos, write)
}

func apiClient(tr http.RoundTripper, baseURL string) (*gh.Client, error) {
	o := []gh.ClientOptionsFunc{gh.WithTransport(tr), gh.WithTimeout(time.Minute)}
	if baseURL != "" {
		base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
		if err != nil {
			return nil, err
		}
		o = append(o, gh.WithURLs(gh.Ptr(base.String()), gh.Ptr(base.String())))
	}
	return gh.NewClient(o...)
}

// New returns a client for the network's repos.
func New(opts Options) (*Client, error) {
	if len(opts.Repos) == 0 && opts.Live == nil {
		return nil, errors.New("github: no repos configured")
	}
	c := &Client{opts: opts, tokens: opts.Tokens, cache: map[string]string{}, expires: map[string]time.Time{}}
	if c.tokens == nil {
		app, err := NewApp(opts.AppID, opts.Key, opts.BaseURL)
		if err != nil {
			return nil, err
		}
		c.tokens = app.For(opts.InstallationID)
	}
	var err error
	if c.gh, err = apiClient(toolsTransport{c}, opts.BaseURL); err != nil {
		return nil, err
	}
	return c, nil
}

// token is a token for repos, reused until it has less than 30 minutes left,
// so a long turn at a desk doesn't outlive it.
func (c *Client) token(ctx context.Context, repos []string, write bool) (string, error) {
	key := fmt.Sprintf("%s %v", strings.Join(repos, ","), write)
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.cache[key]; t != "" && time.Until(c.expires[key]) > 30*time.Minute {
		return t, nil
	}
	t, exp, err := c.tokens.Token(ctx, repos, write)
	if err != nil {
		return "", err
	}
	c.cache[key], c.expires[key] = t, exp
	return t, nil
}

// toolsTransport signs the tools' requests with a token for the network's
// repos and what the tools need.
type toolsTransport struct{ c *Client }

func (t toolsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.c.token(req.Context(), t.c.allRepos(), true)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "token "+token)
	return http.DefaultTransport.RoundTrip(req)
}

// reposFor is the repos p's team may use.
func (c *Client) reposFor(p config.Persona) []string {
	if c.opts.TeamRepos != nil {
		var out []string
		for _, r := range c.opts.TeamRepos(p.Team) {
			if slices.Contains(c.allRepos(), r) {
				out = append(out, r)
			}
		}
		return out
	}
	if c.opts.Teams == nil {
		return c.allRepos()
	}
	for team, repos := range c.opts.Teams {
		if strings.EqualFold(team, p.Team) {
			return repos
		}
	}
	return nil
}

// Checkout is what p's workspace checks out: its team's repos, and a token
// for them that is read-only, limited to those repos, and cached until it
// has less than 30 minutes left, so a long turn at a desk doesn't outlive it.
// No repos means no token.
func (c *Client) Checkout(ctx context.Context, p config.Persona) (urls []string, token string, err error) {
	repos := c.reposFor(p)
	if len(repos) == 0 {
		return nil, "", nil
	}
	for _, r := range repos {
		urls = append(urls, "https://github.com/"+c.owner()+"/"+r)
	}
	t, err := c.token(ctx, repos, false)
	if err != nil {
		return nil, "", fmt.Errorf("github: a read-only token: %w", err)
	}
	return urls, t, nil
}

// Guide tells agents how to work with their team's repos; nothing if it has
// none.
func (c *Client) Guide(p config.Persona) string {
	repos := c.reposFor(p)
	if len(repos) == 0 {
		return ""
	}
	var names, paths []string
	for _, r := range repos {
		names = append(names, c.owner()+"/"+r)
		paths = append(paths, "/workspace/"+r)
	}
	return fmt.Sprintf(`<github>
Your team's repositories (%s) are checked out in your workspace at %s, with a read-only token: read, build, and test there, and git pull for updates, but you can't push from the shell.

To change code, use github_open_pull_request: put your change on a branch of your own, named <your nick>/<topic> (other names are refused). The easiest way: make the change in your workspace and commit it on a branch there, then bring it up to date ("git fetch origin && git rebase origin/<base>"), run the tests on that, and pass patch: the output of "git diff origin/<base>...HEAD" (three dots: only your branch's own changes; for a new PR) or "git diff origin/<your branch>...HEAD" (to add to one). The patch must hold your whole change, not just the latest fix. The PR's checks (github_pr_status) are what count, not tests you ran. Or give files: edits (exact old text and its replacement) for existing files, content only for new ones. Don't resend whole files. Calling it again with the same branch adds a commit to that pull request. If the PR falls behind its base branch, github_update_branch brings it up to date; if that conflicts, redo your change on a new branch from the base and close the old PR with a comment. github_close_pull_request closes one of your PRs, with a comment saying why. github_pr_status shows a PR's checks; github_comment replies on a PR or issue.

Only say something is pushed, opened, closed, or merged when a tool result says so. If a tool failed and your change didn't get in, say that plainly.

Merging (github_merge_pull_request) waits for a person's approval, and merges exactly what they approved. Ask for it when the PR is ready and its checks pass.
</github>`, strings.Join(names, ", "), strings.Join(paths, ", "))
}

// Tools are the GitHub tools p gets: none if its team has no repos.
func (c *Client) Tools(p config.Persona) []agent.Tool {
	repos := c.reposFor(p)
	if len(repos) == 0 {
		return nil
	}
	repo := map[string]any{"type": "string", "enum": repos, "description": "Repository name"}
	number := map[string]any{"type": "integer", "description": "Pull request (or issue) number"}
	return []agent.Tool{
		{
			Name: "github_open_pull_request",
			Description: "Commit files to a branch and open a pull request for it, or add a commit to the branch's open pull request. " +
				"Give either a patch (git diff of what you committed in your workspace) or files (edits for existing files, content for new ones). Only your own <nick>/<topic> branches.",
			Properties: map[string]any{
				"repo":    repo,
				"branch":  map[string]any{"type": "string", "description": "Your branch, e.g. kai/healthz. Created from base if it doesn't exist."},
				"base":    map[string]any{"type": "string", "description": "Branch to merge into; defaults to the repo's default branch"},
				"title":   map[string]any{"type": "string", "description": "Pull request title"},
				"body":    map[string]any{"type": "string", "description": "Pull request description: what and why"},
				"message": map[string]any{"type": "string", "description": "Commit message; defaults to the title"},
				"patch":   map[string]any{"type": "string", "description": "Your whole change as committed in your workspace, rebased on the latest base: the output of `git diff origin/<base>...HEAD` (three dots). Use this or files."},
				"files": map[string]any{"type": "array", "description": "Files to write or delete", "items": map[string]any{
					"type": "object", "required": []string{"path"},
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Path from the repo root"},
						"edits": map[string]any{"type": "array", "description": "Changes to an existing file, applied in order", "items": map[string]any{
							"type": "object", "required": []string{"old", "new"},
							"properties": map[string]any{
								"old": map[string]any{"type": "string", "description": "Exact text in the file, matching one place"},
								"new": map[string]any{"type": "string", "description": "What replaces it"},
							},
						}},
						"content": map[string]any{"type": "string", "description": "A new file's complete content"},
						"delete":  map[string]any{"type": "boolean", "description": "Delete the file instead"},
					},
				}},
			},
			Required: []string{"repo", "branch", "title"},
		},
		{
			Name:        "github_comment",
			Description: "Comment on a pull request or issue.",
			Properties:  map[string]any{"repo": repo, "number": number, "body": map[string]any{"type": "string", "description": "Markdown"}},
			Required:    []string{"repo", "number", "body"},
		},
		{
			Name:        "github_close_pull_request",
			Description: "Close one of your own pull requests without merging, e.g. when it's superseded. Leaves the reason as a comment.",
			Properties:  map[string]any{"repo": repo, "number": number, "comment": map[string]any{"type": "string", "description": "Why it's closed, e.g. \"Superseded by #5\""}},
			Required:    []string{"repo", "number"},
		},
		{
			Name:        "github_update_branch",
			Description: "Bring a pull request's branch up to date with its base branch (merges the base in). Fails if they conflict.",
			Properties:  map[string]any{"repo": repo, "number": number},
			Required:    []string{"repo", "number"},
		},
		{
			Name:        "github_pr_status",
			ReadOnly:    true,
			Description: "A pull request's state, size, and checks.",
			Properties:  map[string]any{"repo": repo, "number": number},
			Required:    []string{"repo", "number"},
		},
		{
			Name:        "github_merge_pull_request",
			Description: "Merge a pull request (squash). Pauses for a person's approval first.",
			Properties:  map[string]any{"repo": repo, "number": number},
			Required:    []string{"repo", "number"},
			Ask:         true,
		},
	}
}

// Step describes a call for IRC, as an action: "* kai opens a pull request
// on guildnet: ...".
func (c *Client) Step(name string, in map[string]any) string {
	repo := str(in, "repo")
	n := fmt.Sprint(in["number"])
	if f, ok := in["number"].(float64); ok {
		n = fmt.Sprint(int(f))
	}
	switch name {
	case "github_open_pull_request":
		return fmt.Sprintf("opens a pull request on %s: %s", repo, str(in, "title"))
	case "github_comment":
		return fmt.Sprintf("comments on %s #%s", repo, n)
	case "github_pr_status":
		return fmt.Sprintf("checks %s PR #%s", repo, n)
	case "github_update_branch":
		return fmt.Sprintf("updates %s PR #%s from its base", repo, n)
	case "github_close_pull_request":
		return fmt.Sprintf("closes %s PR #%s", repo, n)
	case "github_merge_pull_request":
		return fmt.Sprintf("merges %s PR #%s", repo, n)
	}
	return ""
}

// Prepare readies a merge for approval: it checks the PR can be merged,
// describes it for the approver, and pins its head commit.
func (c *Client) Prepare(ctx context.Context, p config.Persona, name string, in map[string]any) (string, map[string]any, error) {
	if name != "github_merge_pull_request" {
		return "", nil, fmt.Errorf("%s doesn't need approval", name)
	}
	repo, n, err := c.repoAndNumber(p, in)
	if err != nil {
		return "", nil, err
	}
	pr, err := c.pull(ctx, repo, n)
	if err != nil {
		return "", nil, err
	}
	if pr.GetMerged() {
		return "", nil, fmt.Errorf("PR #%d is already merged", n)
	}
	if pr.GetState() != "open" {
		return "", nil, fmt.Errorf("PR #%d is %s", n, pr.GetState())
	}
	checks := c.checks(ctx, repo, pr.GetHead().GetSHA())
	summary := fmt.Sprintf("merge PR #%d %q into %s in %s (%d files, +%d -%d; checks: %s)",
		n, pr.GetTitle(), pr.GetBase().GetRef(), repo, pr.GetChangedFiles(), pr.GetAdditions(), pr.GetDeletions(), checks.short())
	pinned := map[string]any{"repo": repo, "number": float64(n), "head_sha": pr.GetHead().GetSHA()}
	return summary, pinned, nil
}

// SourceOf says GitHub's tools read and write the team's code
// (agent.Sourced), so a write elsewhere soon after reading here asks first,
// and a write here after reading elsewhere.
func (c *Client) SourceOf(string) string { return agent.YourRepos }

// Run carries out a GitHub tool call.
func (c *Client) Run(ctx context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	switch name {
	case "github_open_pull_request":
		return c.openPullRequest(ctx, p, in)
	case "github_comment":
		return c.comment(ctx, p, in)
	case "github_pr_status":
		out, err := c.status(ctx, p, in)
		if err != nil {
			return "", err
		}
		return agent.Untrusted("github", out), nil
	case "github_update_branch":
		return c.updateBranch(ctx, p, in)
	case "github_close_pull_request":
		return c.closePull(ctx, p, in)
	case "github_merge_pull_request":
		return c.merge(ctx, p, in)
	}
	return "", fmt.Errorf("no tool %s", name)
}

// closePull closes one of the agent's own PRs: on its branch, or opened by
// it through GuildNet (the footer says so).
func (c *Client) closePull(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, n, err := c.repoAndNumber(p, in)
	if err != nil {
		return "", err
	}
	pr, err := c.pull(ctx, repo, n)
	if err != nil {
		return "", err
	}
	mine := ownBranch(p, pr.GetHead().GetRef()) == nil || strings.Contains(pr.GetBody(), openedBy(p))
	if !mine {
		return "", fmt.Errorf("PR #%d isn't yours to close; ask whoever opened it", n)
	}
	if pr.GetState() != "open" {
		return "", fmt.Errorf("PR #%d is already %s", n, pr.GetState())
	}
	if comment := str(in, "comment"); comment != "" {
		if _, err := c.comment(ctx, p, map[string]any{"repo": repo, "number": float64(n), "body": comment}); err != nil {
			return "", err
		}
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, c.owner(), repo, n, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		return "", apiErr("closing the PR", err)
	}
	return fmt.Sprintf("Closed PR #%d in %s without merging.", n, repo), nil
}

// openedBy is the footer on PRs an agent opens.
func openedBy(p config.Persona) string {
	return fmt.Sprintf("Opened by %s (", p.Nick)
}

func (c *Client) updateBranch(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, n, err := c.repoAndNumber(p, in)
	if err != nil {
		return "", err
	}
	pr, err := c.pull(ctx, repo, n)
	if err != nil {
		return "", err
	}
	if err := ownBranch(p, pr.GetHead().GetRef()); err != nil {
		return "", fmt.Errorf("PR #%d is on %s, which isn't yours to update: %w", n, pr.GetHead().GetRef(), err)
	}
	_, resp, err := c.gh.PullRequests.UpdateBranch(ctx, c.owner(), repo, n, &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: gh.Ptr(pr.GetHead().GetSHA())})
	var accepted *gh.AcceptedError
	switch {
	case errors.As(err, &accepted), err == nil:
		return fmt.Sprintf("Updated PR #%d: merged %s into %s.", n, pr.GetBase().GetRef(), pr.GetHead().GetRef()), nil
	case resp != nil && resp.StatusCode == http.StatusUnprocessableEntity:
		return "", fmt.Errorf("PR #%d conflicts with %s, so it can't be updated automatically (%v). Redo your change on a new branch from %s, open a new PR, and close this one with a comment",
			n, pr.GetBase().GetRef(), apiErr("updating", err), pr.GetBase().GetRef())
	}
	return "", apiErr("updating the branch", err)
}

func (c *Client) openPullRequest(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, err := c.repo(p, in)
	if err != nil {
		return "", err
	}
	branch, title := str(in, "branch"), str(in, "title")
	if branch == "" || title == "" {
		return "", errors.New("branch and title are required")
	}
	if err := ownBranch(p, branch); err != nil {
		return "", err
	}
	message := str(in, "message")
	if message == "" {
		message = title
	}
	changes, err := changesFrom(in)
	if err != nil {
		return "", err
	}
	r, _, err := c.gh.Repositories.Get(ctx, c.owner(), repo)
	if err != nil {
		return "", apiErr("reading "+repo, err)
	}
	base := str(in, "base")
	if base == "" {
		base = r.GetDefaultBranch()
	}
	if branch == base || branch == r.GetDefaultBranch() {
		return "", fmt.Errorf("can't commit straight to %s; use a branch of your own, like %s/<topic>, and open a pull request", branch, strings.ToLower(p.Nick))
	}

	// Build on the branch if it exists, else on base.
	parent, exists, err := c.ref(ctx, repo, branch)
	if err != nil {
		return "", err
	}
	if !exists {
		if parent, _, err = c.ref(ctx, repo, base); err != nil {
			return "", err
		}
		if parent == "" {
			return "", fmt.Errorf("%s has no branch %s", repo, base)
		}
	}
	entries, err := c.treeEntries(ctx, repo, parent, changes)
	if err != nil {
		return "", err
	}
	pc, _, err := c.gh.Git.GetCommit(ctx, c.owner(), repo, parent)
	if err != nil {
		return "", apiErr("reading the branch's latest commit", err)
	}
	tree, _, err := c.gh.Git.CreateTree(ctx, c.owner(), repo, pc.GetTree().GetSHA(), entries)
	if err != nil {
		return "", apiErr("writing the files", err)
	}
	commit, _, err := c.gh.Git.CreateCommit(ctx, c.owner(), repo, gh.Commit{
		Message: gh.Ptr(message),
		Tree:    &gh.Tree{SHA: tree.SHA},
		Parents: []*gh.Commit{{SHA: gh.Ptr(parent)}},
		Author:  &gh.CommitAuthor{Name: gh.Ptr(fmt.Sprintf("%s (%s, AI agent)", p.Name, p.Nick)), Email: gh.Ptr(strings.ToLower(p.Nick) + "@agents.guildnet.dev")},
	}, nil)
	if err != nil {
		return "", apiErr("committing", err)
	}
	if exists {
		_, _, err = c.gh.Git.UpdateRef(ctx, c.owner(), repo, "heads/"+branch, gh.UpdateRef{SHA: commit.GetSHA(), Force: gh.Ptr(false)})
	} else {
		_, _, err = c.gh.Git.CreateRef(ctx, c.owner(), repo, gh.CreateRef{Ref: "refs/heads/" + branch, SHA: commit.GetSHA()})
	}
	if err != nil {
		return "", apiErr("updating "+branch, err)
	}

	open, _, err := c.gh.PullRequests.List(ctx, c.owner(), repo, &gh.PullRequestListOptions{Head: c.owner() + ":" + branch, State: "open"})
	if err != nil {
		return "", apiErr("looking for the branch's pull request", err)
	}
	if len(open) > 0 {
		return fmt.Sprintf("Committed %s to %s; PR #%d is updated: %s", short(commit.GetSHA()), branch, open[0].GetNumber(), open[0].GetHTMLURL()), nil
	}
	body := strings.TrimSpace(str(in, "body")) + fmt.Sprintf("\n\n---\nOpened by %s (%s), an AI agent, via GuildNet.", p.Nick, p.Role)
	pr, _, err := c.gh.PullRequests.Create(ctx, c.owner(), repo, gh.CreatePullRequest{
		Title: gh.Ptr(title), Head: branch, Base: base, Body: gh.Ptr(strings.TrimSpace(body)),
	})
	if err != nil {
		return "", apiErr("opening the pull request", err)
	}
	return fmt.Sprintf("Opened PR #%d (%s into %s): %s", pr.GetNumber(), branch, base, pr.GetHTMLURL()), nil
}

func (c *Client) comment(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, n, err := c.repoAndNumber(p, in)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(str(in, "body"))
	if body == "" {
		return "", errors.New("body is required")
	}
	body += fmt.Sprintf("\n\n— %s (AI agent, via GuildNet)", p.Nick)
	cm, _, err := c.gh.Issues.CreateComment(ctx, c.owner(), repo, n, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return "", apiErr("commenting", err)
	}
	return "Commented: " + cm.GetHTMLURL(), nil
}

func (c *Client) status(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, n, err := c.repoAndNumber(p, in)
	if err != nil {
		return "", err
	}
	pr, err := c.pull(ctx, repo, n)
	if err != nil {
		return "", err
	}
	state := pr.GetState()
	if pr.GetMerged() {
		state = "merged"
	}
	mergeable := "unknown yet"
	if pr.Mergeable != nil {
		mergeable = map[bool]string{true: "yes", false: "no (conflicts)"}[pr.GetMergeable()]
	}
	checks := c.checks(ctx, repo, pr.GetHead().GetSHA())
	return fmt.Sprintf("PR #%d %q: %s, %s into %s at %s\n%d files, +%d -%d; mergeable: %s\nchecks: %s\n%s",
		n, pr.GetTitle(), state, pr.GetHead().GetRef(), pr.GetBase().GetRef(), short(pr.GetHead().GetSHA()),
		pr.GetChangedFiles(), pr.GetAdditions(), pr.GetDeletions(), mergeable, checks.long(), pr.GetHTMLURL()), nil
}

func (c *Client) merge(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	repo, n, err := c.repoAndNumber(p, in)
	if err != nil {
		return "", err
	}
	head := str(in, "head_sha")
	if head == "" {
		return "", errors.New("merging needs a person's approval first, and this call wasn't approved")
	}
	res, resp, err := c.gh.PullRequests.Merge(ctx, c.owner(), repo, n, "", &gh.PullRequestOptions{SHA: head, MergeMethod: "squash"})
	if resp != nil && resp.StatusCode == http.StatusConflict {
		return "", fmt.Errorf("PR #%d changed after it was approved (new commits since %s), so it wasn't merged; ask for approval again", n, short(head))
	}
	if err != nil {
		return "", apiErr("merging", err)
	}
	return fmt.Sprintf("Merged PR #%d in %s (commit %s).", n, repo, short(res.GetSHA())), nil
}

// ClosePullRequest closes a PR and deletes its branch. Not a tool agents
// get; it's for cleaning up after live tests.
func (c *Client) ClosePullRequest(ctx context.Context, repo string, n int) error {
	pr, err := c.pull(ctx, repo, n)
	if err != nil {
		return err
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, c.owner(), repo, n, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		return apiErr("closing the PR", err)
	}
	if _, err := c.gh.Git.DeleteRef(ctx, c.owner(), repo, "heads/"+pr.GetHead().GetRef()); err != nil {
		return apiErr("deleting the branch", err)
	}
	return nil
}

// ref is a branch's commit, and whether the branch exists.
func (c *Client) ref(ctx context.Context, repo, branch string) (string, bool, error) {
	r, resp, err := c.gh.Git.GetRef(ctx, c.owner(), repo, "heads/"+branch)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, apiErr("reading "+branch, err)
	}
	return r.GetObject().GetSHA(), true, nil
}

func (c *Client) pull(ctx context.Context, repo string, n int) (*gh.PullRequest, error) {
	pr, resp, err := c.gh.PullRequests.Get(ctx, c.owner(), repo, n)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s has no PR #%d", repo, n)
	}
	if err != nil {
		return nil, apiErr(fmt.Sprintf("reading PR #%d", n), err)
	}
	return pr, nil
}

// checkList is a commit's check runs, "name: result", or why they couldn't
// be read.
type checkList struct {
	runs []string
	err  error
}

func (c *Client) checks(ctx context.Context, repo, sha string) (out checkList) {
	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, c.owner(), repo, sha, nil)
	if err != nil {
		return checkList{err: apiErr("reading checks", err)}
	}
	for _, r := range runs.CheckRuns {
		result := r.GetConclusion()
		if result == "" {
			result = r.GetStatus()
		}
		out.runs = append(out.runs, r.GetName()+": "+result)
	}
	return out
}

func (cl checkList) long() string {
	if cl.err != nil {
		return "unknown: GuildNet can't read checks (" + cl.err.Error() + "; the GitHub App needs Checks: read)"
	}
	if len(cl.runs) == 0 {
		return "none"
	}
	return strings.Join(cl.runs, ", ")
}

func (cl checkList) short() string {
	if cl.err != nil {
		return "unknown"
	}
	if len(cl.runs) == 0 {
		return "none"
	}
	var passed, failed, waiting int
	for _, c := range cl.runs {
		_, result, _ := strings.Cut(c, ": ")
		switch result {
		case "success", "neutral", "skipped":
			passed++
		case "failure", "cancelled", "timed_out", "action_required", "startup_failure":
			failed++
		default:
			waiting++
		}
	}
	var parts []string
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d FAILING", failed))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d running", waiting))
	}
	if passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", passed))
	}
	return strings.Join(parts, ", ")
}

// repo is the input's repo, if it's one agents can use.
func (c *Client) repo(p config.Persona, in map[string]any) (string, error) {
	r := str(in, "repo")
	if o, name, ok := strings.Cut(r, "/"); ok && strings.EqualFold(o, c.owner()) {
		r = name
	}
	repos := c.reposFor(p)
	if len(repos) == 0 {
		return "", fmt.Errorf("the %s team has no GitHub repos", p.Team)
	}
	if !slices.Contains(repos, r) {
		return "", fmt.Errorf("%q isn't one of your team's repos; use one of: %s", str(in, "repo"), strings.Join(repos, ", "))
	}
	return r, nil
}

func (c *Client) repoAndNumber(p config.Persona, in map[string]any) (string, int, error) {
	repo, err := c.repo(p, in)
	if err != nil {
		return "", 0, err
	}
	var n int
	switch v := in["number"].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case string:
		fmt.Sscanf(strings.TrimPrefix(v, "#"), "%d", &n)
	}
	if n <= 0 {
		return "", 0, errors.New("number is required")
	}
	return repo, n, nil
}

// ownBranch checks a branch is the agent's own, named <nick>/<topic>, so
// agents never push to each other's work, or anyone else's.
func ownBranch(p config.Persona, branch string) error {
	prefix := strings.ToLower(p.Nick) + "/"
	if !strings.HasPrefix(strings.ToLower(branch), prefix) || len(branch) == len(prefix) {
		return fmt.Errorf("your branches must be named %s<topic>, like %shealthz", prefix, prefix)
	}
	return nil
}

// fileChange is one file to write, delete, edit, or patch.
type fileChange struct {
	path    string
	content *string // the whole new content
	remove  bool
	edits   [][2]string   // old, new
	patch   *gitdiff.File // a file's changes from a git patch
}

// changesFrom reads a call's changes: files, or a patch, or both.
func changesFrom(in map[string]any) ([]fileChange, error) {
	var out []fileChange
	if list, _ := in["files"].([]any); len(list) > 0 {
		files, err := fileChanges(list)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	if patch := str(in, "patch"); patch != "" {
		files, _, err := gitdiff.Parse(strings.NewReader(patch + "\n"))
		if err != nil {
			return nil, fmt.Errorf("couldn't read the patch: %w", err)
		}
		if len(files) == 0 {
			return nil, errors.New("the patch has no file changes; give the output of git diff")
		}
		for _, f := range files {
			if f.IsBinary {
				return nil, fmt.Errorf("%s: binary changes aren't supported", f.NewName)
			}
			out = append(out, fileChange{path: f.NewName, patch: f})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("nothing to commit: give files (edits for existing files, content for new ones), or patch: the output of `git diff <base>` in your workspace, to send commits you made there")
	}
	return out, nil
}

func fileChanges(list []any) ([]fileChange, error) {
	var out []fileChange
	for _, f := range list {
		m, _ := f.(map[string]any)
		ch := fileChange{path: strings.TrimPrefix(str(m, "path"), "/")}
		if ch.path == "" || strings.Contains(ch.path, "..") {
			return nil, fmt.Errorf("bad file path %q", str(m, "path"))
		}
		ch.remove, _ = m["delete"].(bool)
		if content, ok := m["content"].(string); ok {
			ch.content = &content
		}
		edits, _ := m["edits"].([]any)
		for _, e := range edits {
			em, _ := e.(map[string]any)
			old, _ := em["old"].(string)
			if old == "" {
				return nil, fmt.Errorf("%s: each edit needs the exact old text to replace", ch.path)
			}
			repl, _ := em["new"].(string)
			ch.edits = append(ch.edits, [2]string{old, repl})
		}
		if !ch.remove && ch.content == nil && len(ch.edits) == 0 {
			return nil, fmt.Errorf("%s needs edits, content, or delete: true", ch.path)
		}
		out = append(out, ch)
	}
	return out, nil
}

// treeEntries turns changes into tree entries, applying edits to each file
// as it is at commit.
func (c *Client) treeEntries(ctx context.Context, repo, commit string, changes []fileChange) ([]*gh.TreeEntry, error) {
	var out []*gh.TreeEntry
	for _, ch := range changes {
		if ch.patch != nil {
			entries, err := c.patched(ctx, repo, commit, ch.patch)
			if err != nil {
				return nil, err
			}
			out = append(out, entries...)
			continue
		}
		e := &gh.TreeEntry{Path: gh.Ptr(ch.path), Mode: gh.Ptr("100644"), Type: gh.Ptr("blob")}
		switch {
		case ch.remove:
		case ch.content != nil:
			e.Content = ch.content
		default:
			content, err := c.fileAt(ctx, repo, commit, ch.path)
			if err != nil {
				return nil, err
			}
			for i, ed := range ch.edits {
				switch n := strings.Count(content, ed[0]); n {
				case 1:
					content = strings.Replace(content, ed[0], ed[1], 1)
				case 0:
					return nil, fmt.Errorf("%s: edit %d's old text isn't in the file (it must match the branch exactly)", ch.path, i+1)
				default:
					return nil, fmt.Errorf("%s: edit %d's old text matches %d places; include more surrounding lines so it matches one", ch.path, i+1, n)
				}
			}
			e.Content = &content
		}
		out = append(out, e)
	}
	return out, nil
}

// patched applies one file's patch to that file as it is at commit.
func (c *Client) patched(ctx context.Context, repo, commit string, f *gitdiff.File) ([]*gh.TreeEntry, error) {
	blob := func(path string) *gh.TreeEntry {
		return &gh.TreeEntry{Path: gh.Ptr(path), Mode: gh.Ptr("100644"), Type: gh.Ptr("blob")}
	}
	if f.IsDelete {
		return []*gh.TreeEntry{blob(f.OldName)}, nil
	}
	var src string
	if !f.IsNew {
		var err error
		if src, err = c.fileAt(ctx, repo, commit, f.OldName); err != nil {
			return nil, err
		}
	}
	var dst bytes.Buffer
	if err := gitdiff.Apply(&dst, strings.NewReader(src), f); err != nil {
		return nil, fmt.Errorf("%s: the patch doesn't apply to the branch (%v); make it with git diff against the branch as it is now", f.NewName, err)
	}
	e := blob(f.NewName)
	e.Content = gh.Ptr(dst.String())
	out := []*gh.TreeEntry{e}
	if f.IsRename {
		out = append(out, blob(f.OldName))
	}
	return out, nil
}

// fileAt is a file's content at a commit.
func (c *Client) fileAt(ctx context.Context, repo, commit, path string) (string, error) {
	f, _, resp, err := c.gh.Repositories.GetContents(ctx, c.owner(), repo, path, &gh.RepositoryContentGetOptions{Ref: commit})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s doesn't exist on the branch; to create it, give its content", path)
	}
	if err != nil {
		return "", apiErr("reading "+path, err)
	}
	if f == nil {
		return "", fmt.Errorf("%s is a directory", path)
	}
	return f.GetContent()
}

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return strings.TrimSpace(s)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// apiErr keeps GitHub's own message, which is usually the useful part.
func apiErr(doing string, err error) error {
	var e *gh.ErrorResponse
	if errors.As(err, &e) {
		return fmt.Errorf("%s: GitHub said %q", doing, e.Message)
	}
	return fmt.Errorf("%s: %w", doing, err)
}

// owner is the account the repos are on.
func (c *Client) owner() string {
	if c.opts.Live != nil {
		account, _ := c.opts.Live.Get()
		return account
	}
	return c.opts.Owner
}

// allRepos are the network's repos: what its installation has.
func (c *Client) allRepos() []string {
	if c.opts.Live != nil {
		_, repos := c.opts.Live.Get()
		return repos
	}
	return c.opts.Repos
}
