// SPDX-License-Identifier: AGPL-3.0-only

package gitremote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/gitremote"
	"github.com/guildnet-chat/guildnet/internal/gittest"
)

// An agent at a desk pushes a branch of its work to one of its team's git
// repositories with git_push_branch: the desk makes a pack of the branch's
// new commits, and the runner pushes it with the repository's push
// credential, which never reaches the desk. Here the desk is this machine:
// its /workspace a folder of the test's, its commands run in bash with git,
// as at a real desk.

type localDesk struct {
	root, tmp, bash string
	env             []string
}

func newLocalDesk(t *testing.T, s *gittest.Server) *localDesk {
	t.Helper()
	bash := "bash"
	if runtime.GOOS == "windows" {
		// Git's own bash, not WSL's.
		out, err := exec.Command("git", "--exec-path").Output()
		if err != nil {
			t.Skip("no git here")
		}
		bash = filepath.Join(strings.TrimSpace(string(out)), "..", "..", "..", "bin", "bash.exe")
	}
	if _, err := exec.LookPath(bash); err != nil {
		t.Skip("no bash here")
	}
	d := &localDesk{root: filepath.ToSlash(t.TempDir()), tmp: filepath.ToSlash(t.TempDir()), bash: bash, env: s.Env()}
	return d
}

// here is a desk's path (or a script with its paths in it) on this machine.
func (d *localDesk) here(s string) string {
	if rest, ok := strings.CutPrefix(s, "/tmp/"); ok {
		return d.tmp + "/" + rest
	}
	return strings.ReplaceAll(s, "/workspace", d.root)
}

func (d *localDesk) Run(_ context.Context, _ string, c desk.Command) (desk.Output, error) {
	var args []string
	for _, a := range c.Args[1:] {
		args = append(args, d.here(a))
	}
	if c.Args[0] != "bash" {
		// Through bash, which has the desk's commands on its PATH.
		args = append([]string{"-c", `"$@"`, c.Args[0], c.Args[0]}, args...)
	}
	cmd := exec.Command(d.bash, args...)
	cmd.Dir, cmd.Env = d.here(c.Dir), d.env
	cmd.Stdin = strings.NewReader(c.Stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return desk.Output{Text: out.String(), Exit: exit.ExitCode()}, nil
	}
	return desk.Output{Text: out.String()}, err
}

func (d *localDesk) Read(_ context.Context, _ string, path string, max int64) ([]byte, error) {
	b, err := os.ReadFile(d.here(path))
	if err != nil {
		return nil, desk.ErrNoFile
	}
	if int64(len(b)) > max {
		return nil, desk.ErrTooBig
	}
	return b, nil
}

func (d *localDesk) Open(context.Context, string) error                  { return nil }
func (d *localDesk) Write(context.Context, string, string, []byte) error { return nil }
func (d *localDesk) Remove(context.Context, string) error                { return nil }

// fakeBroker is the platform's broker for one network: infra may be pushed
// to, docs only read.
func fakeBroker(t *testing.T, s *gittest.Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /git", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"repos": []gitremote.Repo{
			{Name: "infra", URL: s.URL(), Read: true, Write: true},
			{Name: "docs", URL: strings.Replace(s.URL(), "repo.git", "docs.git", 1), Read: true},
		}})
	})
	mux.HandleFunc("POST /git/infra/write", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"url": s.URL(), "user": gittest.User, "token": gittest.WriteToken})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var kai = config.Persona{Nick: "kai", Name: "Kai", Team: "Engineering"}

type pushed struct {
	mu   sync.Mutex
	list []string
}

func (p *pushed) note(who config.Persona, repo, branch, commit string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.list = append(p.list, who.Nick+" "+repo+" "+branch+" "+commit)
}

// rig is a git server, kai's desk with infra checked out, and the tool.
func rig(t *testing.T) (*gittest.Server, *gittest.Work, *gitremote.Pusher, *pushed) {
	t.Helper()
	s := gittest.New(t)
	d := newLocalDesk(t, s)
	work := s.CloneTo(d.root + "/infra")
	live := &gitremote.Live{Broker: gitremote.Broker{URL: fakeBroker(t, s).URL, Network: "acme", Secret: "s"}}
	if err := live.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := &pushed{}
	p := &gitremote.Pusher{Live: live, Desk: d, HTTP: s.Client(), Pushed: log.note,
		TeamRepos: func(string) []string { return []string{"git:infra", "git:docs", "web"} }}
	return s, work, p, log
}

func push(p *gitremote.Pusher, repo, branch string) (string, error) {
	return p.Run(context.Background(), kai, "git_push_branch", map[string]any{"repo": repo, "branch": branch})
}

func TestAnAgentPushesItsBranch(t *testing.T) {
	s, work, p, log := rig(t)
	work.Git("checkout", "-q", "-b", "kai/fix-build")
	tip := work.Commit("", "fix.txt", "fixed\n")

	out, err := push(p, "infra", "kai/fix-build")

	if err != nil {
		t.Fatal(err)
	}
	if got := s.Ref("refs/heads/kai/fix-build"); got != tip {
		t.Errorf("kai/fix-build is %q on the host, want %s", got, tip)
	}
	if got := s.Ref("refs/heads/main"); got != s.Main {
		t.Errorf("main moved to %s", got)
	}
	if !strings.Contains(out, "kai/fix-build") || !strings.Contains(out, tip[:7]) {
		t.Errorf("the agent was told %q", out)
	}
	if len(log.list) != 1 || log.list[0] != "kai infra kai/fix-build "+tip {
		t.Errorf("the org log has %v", log.list)
	}

	// And again, once there's more.
	more := work.Commit("", "fix.txt", "fixed again\n")
	if _, err := push(p, "git:infra", "kai/fix-build"); err != nil || s.Ref("refs/heads/kai/fix-build") != more {
		t.Errorf("pushing more: %v; the branch is at %s, want %s", err, s.Ref("refs/heads/kai/fix-build"), more)
	}
}

// What the host says (where to open a merge request) comes back, as the
// host's words.
func TestWhatTheHostSaysAboutAPushComesBack(t *testing.T) {
	s, work, p, _ := rig(t)
	s.Hook("post-receive", `echo "To create a merge request, visit: https://git.example.com/infra/new"`)
	work.Git("checkout", "-q", "-b", "kai/x")
	work.Commit("", "x.txt", "x\n")

	out, err := push(p, "infra", "kai/x")

	if err != nil || !strings.Contains(out, "https://git.example.com/infra/new") || !strings.Contains(out, "<untrusted") {
		t.Errorf("the agent was told %q, %v; want the host's words, marked as not ours", out, err)
	}
}

// An agent pushes only its own branches (<nick>/<topic>), never the
// repository's default branch, and only to its team's repositories that
// may be pushed to; nothing reaches the host otherwise.
func TestAnAgentPushesOnlyItsOwnBranchesToItsTeamsRepositories(t *testing.T) {
	s, work, p, _ := rig(t)
	s.SetHead("refs/heads/kai/main")
	work.Git("checkout", "-q", "-b", "kai/x")
	work.Commit("", "x.txt", "x\n")
	before := s.Requests
	for repo, branch := range map[string]string{"infra": "main", "git:infra": "dana/x", "git:docs": "kai/x", "web": "kai/x", "other": "kai/x"} {
		if out, err := push(p, repo, branch); err == nil {
			t.Errorf("pushing %s to %s: %q", branch, repo, out)
		}
	}
	for _, branch := range []string{"kai/", "kai", "refs/heads/kai/x", "kai/../main", "kai/x y"} {
		if out, err := push(p, "infra", branch); err == nil {
			t.Errorf("pushing %q: %q", branch, out)
		}
	}
	if s.Requests != before {
		t.Errorf("the host had %d requests", s.Requests-before)
	}
	s.Clone().Send("refs/heads/kai/main", s.Main)
	work.Git("checkout", "-q", "-b", "kai/main")
	if out, err := push(p, "infra", "kai/main"); err == nil || !strings.Contains(err.Error(), "default") {
		t.Errorf("pushing the default branch: %q, %v", out, err)
	}
}

// A branch on the host with commits the desk doesn't have (someone else
// pushed to it) isn't pushed over: the agent fetches and rebases first.
func TestAPushNeverDropsSomeoneElsesCommits(t *testing.T) {
	s, work, p, _ := rig(t)
	theirs := s.Clone()
	theirTip := theirs.Commit(s.Main, "theirs.txt", "theirs\n")
	theirs.Send("refs/heads/kai/x", theirTip)
	work.Git("checkout", "-q", "-b", "kai/x")
	work.Commit("", "ours.txt", "ours\n")

	_, err := push(p, "infra", "kai/x")

	if err == nil || !strings.Contains(err.Error(), "fetch") {
		t.Errorf("pushing over someone else's commits: %v, want it refused, saying to fetch", err)
	}
	if got := s.Ref("refs/heads/kai/x"); got != theirTip {
		t.Errorf("kai/x is %s, want theirs, %s", got, theirTip)
	}
}

// A branch that isn't at the desk is said so; so is a pack too big.
func TestAPushSaysWhatsWrongAtTheDesk(t *testing.T) {
	_, work, p, _ := rig(t)
	if _, err := push(p, "infra", "kai/nothing"); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("a branch that isn't there: %v", err)
	}
	work.Git("checkout", "-q", "-b", "kai/big")
	work.Commit("", "big.txt", strings.Repeat("big data ", 1000))
	p.MaxPack = 100
	if _, err := push(p, "infra", "kai/big"); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Errorf("a pack too big: %v", err)
	}
}

// The tool is there only for an agent whose team has a repository it may
// push to.
func TestTheToolIsThereOnlyWhereTheresSomewhereToPush(t *testing.T) {
	_, _, p, _ := rig(t)
	if tools := p.Tools(kai); len(tools) != 1 || tools[0].Name != "git_push_branch" || tools[0].ReadOnly || tools[0].Ask {
		t.Errorf("kai's tools: %+v", tools)
	}
	p.TeamRepos = func(string) []string { return []string{"git:docs"} }
	if tools := p.Tools(kai); len(tools) != 0 {
		t.Errorf("with only a repository to read: %+v", tools)
	}
}
