// SPDX-License-Identifier: AGPL-3.0-only

package desk_test

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/gittest"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
)

// The runner checks out and fetches a desk's repos for it: desks can't reach
// git hosts, and never hold a git credential, so something an agent read
// can't steer it into pushing the company's code somewhere else. Here the
// desk is a folder on this machine, its commands run by the system's sh as
// sandboxd would run them, against a real git server.

// hostDesk is a desk on this machine: /workspace is a folder here, and so is
// its home.
type hostDesk struct {
	t          *testing.T
	root, home string
	env        []string
	mu         sync.Mutex
	ran        []desk.Command
	input      []string // everything any command was given on its input
}

func newHostDesk(t *testing.T, s *gittest.Server) *hostDesk {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh here")
	}
	h := &hostDesk{t: t, root: filepath.ToSlash(t.TempDir()), home: filepath.ToSlash(t.TempDir())}
	h.env = append(s.Env(), "HOME="+h.home)
	return h
}

func (h *hostDesk) here(s string) string { return strings.ReplaceAll(s, desk.Root, h.root) }

func (h *hostDesk) exec(c desk.Command, in io.Reader) (desk.Output, error) {
	args := c.Args
	if len(args) > 3 && args[0] == "bash" && args[1] == "--noprofile" {
		args = append([]string{"sh"}, args[3:]...) // bash --noprofile --norc -c script ...
	}
	cmd := exec.Command(args[0])
	for _, a := range args[1:] {
		cmd.Args = append(cmd.Args, h.here(a))
	}
	cmd.Dir, cmd.Env = h.here(c.Dir), h.env
	var got bytes.Buffer
	cmd.Stdin = io.TeeReader(in, &got)
	out, err := cmd.CombinedOutput()
	h.mu.Lock()
	h.ran = append(h.ran, c)
	h.input = append(h.input, got.String())
	h.mu.Unlock()
	if exit, ok := err.(*exec.ExitError); ok {
		return desk.Output{Text: string(out), Exit: exit.ExitCode()}, nil
	}
	return desk.Output{Text: string(out)}, err
}

func (h *hostDesk) Open(context.Context, string) error { return nil }
func (h *hostDesk) Run(_ context.Context, _ string, c desk.Command) (desk.Output, error) {
	return h.exec(c, strings.NewReader(c.Stdin))
}
func (h *hostDesk) Pipe(_ context.Context, _ string, c desk.Command, in io.Reader) (desk.Output, error) {
	return h.exec(c, in)
}
func (h *hostDesk) Read(_ context.Context, _, path string, _ int64) ([]byte, error) {
	return os.ReadFile(h.here(path))
}
func (h *hostDesk) Write(_ context.Context, _, path string, data []byte) error {
	return os.WriteFile(h.here(path), data, 0o644)
}
func (h *hostDesk) Remove(context.Context, string) error { return nil }

// git runs git in a folder at the desk.
func (h *hostDesk) git(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = h.here(dir), h.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// holds says whether anything at the desk, workspace or home, has s in it.
func (h *hostDesk) holds(s string) bool {
	found := false
	for _, root := range []string{h.root, h.home} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(s)) {
					found = true
				}
			}
			return nil
		})
	}
	return found
}

var reader = gitwire.Credential{User: gittest.User, Token: gittest.ReadToken}

func gitDesk(t *testing.T) (*gittest.Server, *hostDesk, *desk.Workspace) {
	t.Helper()
	s := gittest.New(t)
	h := newHostDesk(t, s)
	w := desk.New(h, nil)
	w.Git = fakeGit{{Name: "web", URL: s.URL(), Read: reader}}
	w.HTTP = s.Client()
	return s, h, w
}

var kaiAtDesk = config.Persona{Nick: "kai", Name: "Kai", Team: "Engineering"}

// Readying a desk checks out the team's repos, on their default branch, as
// a clone would; the read credential never reaches the desk.
func TestReadyingADeskChecksOutTheTeamsReposWithoutTheirCredentials(t *testing.T) {
	s, h, w := gitDesk(t)

	if err := w.Ready(context.Background(), kaiAtDesk); err != nil {
		t.Fatal(err)
	}

	if got := h.git("/workspace/web", "rev-parse", "HEAD"); got != s.Main {
		t.Errorf("the checkout is at %q, want main's %s", got, s.Main)
	}
	if got := h.git("/workspace/web", "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("the checkout is on %q, want main", got)
	}
	if got := h.git("/workspace/web", "rev-parse", "--abbrev-ref", "main@{upstream}"); got != "origin/main" {
		t.Errorf("main tracks %q, want origin/main", got)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "web", "README")); string(got) != "hello\n" {
		t.Errorf("README at the desk: %q", got)
	}
	if got := h.git("/workspace/web", "config", "--get", "remote.origin.url"); got != s.URL() {
		t.Errorf("origin is %q, want the repository's address", got)
	}
	if h.holds(gittest.ReadToken) {
		t.Error("the read credential is somewhere at the desk")
	}
	for i, c := range h.ran {
		if strings.Contains(strings.Join(c.Args, " "), gittest.ReadToken) || strings.Contains(h.input[i], gittest.ReadToken) {
			t.Errorf("a command at the desk was given the credential: %v", c.Args)
		}
	}
}

// git_fetch brings what's new on the host as origin/*, as git fetch would:
// the agent's own branch stays where it was, and a branch gone from the host
// goes from origin/* too.
func TestGitFetchBringsWhatsNewAsOrigin(t *testing.T) {
	s, h, w := gitDesk(t)
	other := s.Clone()
	gone := other.Commit(s.Main, "old.txt", "old\n")
	other.Send("refs/heads/old", gone)
	if err := w.Ready(context.Background(), kaiAtDesk); err != nil {
		t.Fatal(err)
	}
	if got := h.git("/workspace/web", "rev-parse", "origin/old"); got != gone {
		t.Fatalf("origin/old is %q", got)
	}
	next := other.Commit(s.Main, "news.txt", "fresh\n")
	other.Send("refs/heads/main", next)
	other.Git("push", "-q", "origin", ":refs/heads/old")

	out, err := w.Run(context.Background(), kaiAtDesk, "git_fetch", map[string]any{})

	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/workspace/web") || !strings.Contains(out, next[:7]) {
		t.Errorf("git_fetch said %q", out)
	}
	if got := h.git("/workspace/web", "rev-parse", "origin/main"); got != next {
		t.Errorf("origin/main is %q, want %s", got, next)
	}
	if got := h.git("/workspace/web", "rev-parse", "main"); got != s.Main {
		t.Errorf("the desk's own main moved to %q: a fetch doesn't merge", got)
	}
	if got := h.git("/workspace/web", "branch", "-r"); strings.Contains(got, "origin/old") {
		t.Errorf("origin/old is still there:\n%s", got)
	}
}

// Fetching again with nothing new sends nothing: the desk said what it has.
func TestNothingNewFetchesNothing(t *testing.T) {
	s, h, w := gitDesk(t)
	w.Ready(context.Background(), kaiAtDesk)
	before := len(h.ran)

	if _, err := w.Run(context.Background(), kaiAtDesk, "git_fetch", map[string]any{"repo": "web"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range h.ran[before:] {
		if strings.Contains(strings.Join(c.Args, " "), "index-pack") {
			t.Error("a pack was sent with nothing new on the host")
		}
	}
	if got := h.git("/workspace/web", "rev-parse", "origin/main"); got != s.Main {
		t.Errorf("origin/main is %q", got)
	}
}

// A repository whose credential the host refuses isn't checked out, and the
// desk isn't ready, saying why.
func TestACheckoutTheHostRefusesSaysWhy(t *testing.T) {
	s, _, w := gitDesk(t)
	w.Git = fakeGit{{Name: "web", URL: s.URL(), Read: gitwire.Credential{User: gittest.User, Token: "wrong"}}}

	err := w.Ready(context.Background(), kaiAtDesk)

	if err == nil || !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "refused") {
		t.Errorf("err = %v", err)
	}
}
