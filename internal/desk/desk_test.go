// SPDX-License-Identifier: AGPL-3.0-only

package desk_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
)

// An agent at a desk of GuildNet's own works in its workspace through six
// tools (bash, read, write, edit, glob, grep), which the runner carries out
// in the agent's sandbox. The sandbox here is a stand-in: files in memory,
// and commands answered as a test says.

type fakeSandbox struct {
	mu      sync.Mutex
	files   map[string]string // "nick path" -> content
	ran     []desk.Command
	opened  []string
	removed []string
	answer  func(desk.Command) desk.Output
	openErr error
}

func newSandbox() *fakeSandbox {
	return &fakeSandbox{files: map[string]string{}}
}

func (f *fakeSandbox) Open(_ context.Context, nick string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, nick)
	return f.openErr
}

func (f *fakeSandbox) Run(_ context.Context, nick string, c desk.Command) (desk.Output, error) {
	f.mu.Lock()
	f.ran = append(f.ran, c)
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		return desk.Output{}, nil
	}
	return answer(c), nil
}

func (f *fakeSandbox) Read(_ context.Context, nick, path string, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.files[nick+" "+path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, desk.ErrNoFile)
	}
	if int64(len(s)) > max {
		return nil, fmt.Errorf("%s: %w", path, desk.ErrTooBig)
	}
	return []byte(s), nil
}

func (f *fakeSandbox) Write(_ context.Context, nick, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[nick+" "+path] = string(data)
	return nil
}

func (f *fakeSandbox) Remove(_ context.Context, nick string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, nick)
	return nil
}

func (f *fakeSandbox) commands() []desk.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]desk.Command(nil), f.ran...)
}

// fakeRepos is the team's GitHub: repos to check out, and a read-only token.
type fakeRepos struct {
	urls  []string
	token string
}

func (r fakeRepos) Checkout(context.Context, config.Persona) ([]string, string, error) {
	return r.urls, r.token, nil
}

var kai = config.Persona{Nick: "kai", Team: "Engineering"}

// fromDesk is what an agent is given of out from its desk: marked as content
// from there, which anyone may have written, not instructions.
func fromDesk(out string) string { return agent.Untrusted("desk", out) }

func toolNames(w *desk.Workspace) []string {
	var names []string
	for _, t := range w.Tools(kai) {
		names = append(names, t.Name)
	}
	return names
}

func TestAnAgentAtADeskHasSixWorkspaceTools(t *testing.T) {
	w := desk.New(newSandbox(), nil)

	names := toolNames(w)

	for _, want := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v, missing %s", names, want)
		}
	}
	for _, tool := range w.Tools(kai) {
		if tool.Ask {
			t.Errorf("%s asks a person: work inside the workspace shouldn't", tool.Name)
		}
		if !tool.ReadOnly {
			t.Errorf("%s's results would be announced in IRC; the step is enough", tool.Name)
		}
	}
}

func TestBashRunsACommandInTheWorkspace(t *testing.T) {
	sb := newSandbox()
	sb.answer = func(desk.Command) desk.Output { return desk.Output{Text: "ok  guildnet/internal/brain\n"} }
	w := desk.New(sb, nil)

	out, err := w.Run(context.Background(), kai, "bash", map[string]any{"command": "go test ./internal/brain/"})

	if err != nil || out != fromDesk("ok  guildnet/internal/brain\n") {
		t.Fatalf("got %q, %v", out, err)
	}
	c := sb.commands()[0]
	if !slices.Equal(c.Args, []string{"bash", "--noprofile", "--norc", "-c", "go test ./internal/brain/"}) || c.Dir != "/workspace" {
		t.Errorf("ran %v in %q", c.Args, c.Dir)
	}
	if c.Timeout != 2*time.Minute {
		t.Errorf("timeout %v, want 2 minutes unless the agent asks for more", c.Timeout)
	}
}

// Out of space, the agent is told why, by GuildNet (outside what its desk
// said), and what to do: the network's desks' storage is full.
func TestADeskOutOfSpaceIsToldTheStorageIsFull(t *testing.T) {
	for _, said := range []string{"cp: error writing 'big.bin': No space left on device", "write error: Disk quota exceeded"} {
		sb := newSandbox()
		sb.answer = func(desk.Command) desk.Output { return desk.Output{Text: said + "\n", Exit: 1} }
		w := desk.New(sb, nil)

		out, err := w.Run(context.Background(), kai, "bash", map[string]any{"command": "cp big.bin copy.bin"})

		if err != nil || !strings.HasPrefix(out, fromDesk(said+"\n\n[exit status 1]")) {
			t.Fatalf("got %q, %v", out, err)
		}
		if note := strings.TrimPrefix(out, fromDesk(said+"\n\n[exit status 1]")); !strings.Contains(note, "storage is full") || !strings.Contains(note, "owner") {
			t.Errorf("after %q, the agent isn't told the storage is full and to tell the owner: %q", said, note)
		}
	}
	sb := newSandbox()
	sb.answer = func(desk.Command) desk.Output { return desk.Output{Text: "FAIL\n", Exit: 1} }
	if out, _ := desk.New(sb, nil).Run(context.Background(), kai, "bash", map[string]any{"command": "go test ./..."}); strings.Contains(out, "storage is full") {
		t.Errorf("an ordinary failure says the storage is full: %q", out)
	}
}

func TestACommandThatFailsSaysSo(t *testing.T) {
	sb := newSandbox()
	sb.answer = func(desk.Command) desk.Output { return desk.Output{Text: "FAIL\n", Exit: 1} }
	w := desk.New(sb, nil)

	out, err := w.Run(context.Background(), kai, "bash", map[string]any{"command": "go test ./..."})

	if err != nil || !strings.Contains(out, "FAIL") || !strings.Contains(out, "exit status 1") {
		t.Errorf("got %q, %v; want the output and the exit status", out, err)
	}
}

func TestACommandMayRunForUpToTenMinutes(t *testing.T) {
	sb := newSandbox()
	sb.answer = func(desk.Command) desk.Output { return desk.Output{Text: "building", TimedOut: true} }
	w := desk.New(sb, nil)

	out, _ := w.Run(context.Background(), kai, "bash", map[string]any{"command": "make", "timeout": float64(3600)})

	if got := sb.commands()[0].Timeout; got != 10*time.Minute {
		t.Errorf("timeout %v, want it held to 10 minutes", got)
	}
	if !strings.Contains(out, "building") || !strings.Contains(out, "stopped") {
		t.Errorf("got %q; want what it printed, and that it was stopped", out)
	}
}

func TestReadShowsAFilesLinesNumbered(t *testing.T) {
	sb := newSandbox()
	sb.files["kai /workspace/guildnet/go.mod"] = "module guildnet\n\ngo 1.26\n"
	w := desk.New(sb, nil)

	out, err := w.Run(context.Background(), kai, "read", map[string]any{"path": "guildnet/go.mod"})

	if err != nil || out != fromDesk("     1\tmodule guildnet\n     2\t\n     3\tgo 1.26\n") {
		t.Errorf("got %q, %v", out, err)
	}
}

func TestReadCanShowPartOfAFile(t *testing.T) {
	sb := newSandbox()
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	sb.files["kai /workspace/big.txt"] = strings.Join(lines, "\n")
	w := desk.New(sb, nil)

	out, err := w.Run(context.Background(), kai, "read", map[string]any{"path": "/workspace/big.txt", "offset": float64(50), "limit": float64(2)})

	if err != nil || !strings.HasPrefix(out, "<untrusted source=\"desk\">\n    50\tline 50\n    51\tline 51\n[") || !strings.Contains(out, "from line 52") {
		t.Errorf("got %q, %v; want the two lines, and where the rest starts", out, err)
	}
}

func TestReadingAMissingFileIsAFailedCall(t *testing.T) {
	w := desk.New(newSandbox(), nil)

	_, err := w.Run(context.Background(), kai, "read", map[string]any{"path": "nope.go"})

	if err == nil || !strings.Contains(err.Error(), "nope.go") {
		t.Errorf("err = %v, want it to name the file", err)
	}
}

func TestWriteCreatesAFile(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, nil)

	out, err := w.Run(context.Background(), kai, "write", map[string]any{"path": "notes.md", "content": "# Plan\n"})

	if err != nil || sb.files["kai /workspace/notes.md"] != "# Plan\n" || !strings.Contains(out, "notes.md") {
		t.Errorf("got %q, %v; files %v", out, err, sb.files)
	}
}

func TestEditReplacesExactlyOnePlace(t *testing.T) {
	sb := newSandbox()
	sb.files["kai /workspace/main.go"] = "func a() {}\nfunc b() {}\n"
	w := desk.New(sb, nil)

	_, err := w.Run(context.Background(), kai, "edit", map[string]any{"path": "main.go", "old": "func b() {}", "new": "func b() { a() }"})

	if err != nil || sb.files["kai /workspace/main.go"] != "func a() {}\nfunc b() { a() }\n" {
		t.Errorf("file %q, %v", sb.files["kai /workspace/main.go"], err)
	}
}

func TestAnEditThatDoesntMatchOnePlaceIsRefused(t *testing.T) {
	sb := newSandbox()
	sb.files["kai /workspace/main.go"] = "x := 1\nx := 1\n"
	w := desk.New(sb, nil)
	ctx := context.Background()

	_, twice := w.Run(ctx, kai, "edit", map[string]any{"path": "main.go", "old": "x := 1", "new": "x := 2"})
	_, missing := w.Run(ctx, kai, "edit", map[string]any{"path": "main.go", "old": "y := 1", "new": "y := 2"})
	_, all := w.Run(ctx, kai, "edit", map[string]any{"path": "main.go", "old": "x := 1", "new": "x := 2", "replace_all": true})

	if twice == nil || !strings.Contains(twice.Error(), "2 places") {
		t.Errorf("text in 2 places: %v", twice)
	}
	if missing == nil || !strings.Contains(missing.Error(), "isn't in") {
		t.Errorf("missing text: %v", missing)
	}
	if all != nil || sb.files["kai /workspace/main.go"] != "x := 2\nx := 2\n" {
		t.Errorf("replace_all: %q, %v", sb.files["kai /workspace/main.go"], all)
	}
}

func TestGlobAndGrepSearchTheWorkspace(t *testing.T) {
	sb := newSandbox()
	sb.answer = func(c desk.Command) desk.Output {
		if c.Args[0] == "rg" && slices.Contains(c.Args, "--files") {
			return desk.Output{Text: "internal/brain/loop.go\ninternal/brain/desks.go\n"}
		}
		return desk.Output{Text: "internal/brain/loop.go:20:\tchatRounds = 20\n"}
	}
	w := desk.New(sb, nil)
	ctx := context.Background()

	files, err1 := w.Run(ctx, kai, "glob", map[string]any{"pattern": "**/*.go", "path": "guildnet"})
	hits, err2 := w.Run(ctx, kai, "grep", map[string]any{"pattern": "chatRounds", "path": "guildnet", "glob": "*.go", "ignore_case": true})

	if err1 != nil || !strings.Contains(files, "internal/brain/desks.go") {
		t.Errorf("glob: %q, %v", files, err1)
	}
	if err2 != nil || !strings.Contains(hits, "chatRounds = 20") {
		t.Errorf("grep: %q, %v", hits, err2)
	}
	ran := sb.commands()
	if g := ran[0]; !slices.Contains(g.Args, "**/*.go") || g.Args[len(g.Args)-1] != "/workspace/guildnet" {
		t.Errorf("glob ran %v", g.Args)
	}
	if g := ran[1]; !slices.Contains(g.Args, "-i") || !slices.Contains(g.Args, "*.go") || !slices.Contains(g.Args, "chatRounds") || g.Args[len(g.Args)-1] != "/workspace/guildnet" {
		t.Errorf("grep ran %v", g.Args)
	}
}

// A pattern that starts with a dash is still a pattern, never an option.
func TestAGrepPatternCantBecomeAnOption(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, nil)

	w.Run(context.Background(), kai, "grep", map[string]any{"pattern": "--pre=sh"})

	args := sb.commands()[0].Args
	i := slices.Index(args, "--pre=sh")
	if i < 1 || args[i-1] != "--" {
		t.Errorf("ran %v; the pattern should come after --", args)
	}
}

func TestNothingFoundIsSaidPlainly(t *testing.T) {
	sb := newSandbox()
	sb.answer = func(desk.Command) desk.Output { return desk.Output{Exit: 1} }
	w := desk.New(sb, nil)

	out, err := w.Run(context.Background(), kai, "grep", map[string]any{"pattern": "nothing"})

	if err != nil || out != fromDesk("No matches.") {
		t.Errorf("got %q, %v", out, err)
	}
}

func TestStepsSayWhatTheAgentDoes(t *testing.T) {
	w := desk.New(newSandbox(), nil)

	for name, want := range map[string]string{
		"bash:command=go test ./...": "runs go test ./...",
		"read:path=main.go":          "reads main.go",
		"edit:path=main.go":          "edits main.go",
		"grep:pattern=chatRounds":    "searches files for chatRounds",
	} {
		tool, kv, _ := strings.Cut(name, ":")
		k, v, _ := strings.Cut(kv, "=")
		if got := w.Step(tool, map[string]any{k: v}); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// Readying a desk wakes its sandbox and checks out the team's repos with a
// read-only token, which goes in on the command's input, not its arguments
// (where process lists and logs would show it).
func TestReadyingADeskChecksOutTheTeamsRepos(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, fakeRepos{urls: []string{"https://github.com/acme/api", "https://github.com/acme/web"}, token: "ghs_readonly"})

	if err := w.Ready(context.Background(), kai); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(sb.opened, []string{"kai"}) {
		t.Errorf("opened %v", sb.opened)
	}
	ran := sb.commands()
	if len(ran) != 1 {
		t.Fatalf("ran %d commands", len(ran))
	}
	c := ran[0]
	if !strings.Contains(c.Stdin, "ghs_readonly") || strings.Contains(strings.Join(c.Args, " "), "ghs_") {
		t.Errorf("the token went in %q / %v; want only on the input", c.Stdin, c.Args)
	}
	if !slices.Contains(c.Args, "https://github.com/acme/api") || !slices.Contains(c.Args, "https://github.com/acme/web") {
		t.Errorf("ran %v; want both repos checked out", c.Args)
	}
}

// fakeGit is the team's git repositories on other hosts.
type fakeGit []desk.Clone

func (g fakeGit) Clones(context.Context, config.Persona) ([]desk.Clone, error) { return g, nil }

// Git repositories on any host are checked out too, each with its own read
// credential, on the command's input; desks reach their hosts with the
// network's egress pass, also on the input.
func TestReadyingADeskChecksOutTheTeamsGitRepositories(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, fakeRepos{urls: []string{"https://github.com/acme/web"}, token: "ghs_readonly"})
	w.Git = fakeGit{
		{Name: "infra", URL: "https://git.example.com/ops/infra.git", Read: gitwire.Credential{User: "deploy", Token: "glpat-infra"}},
		{Name: "web", URL: "https://git.example.com/ops/web.git", Read: gitwire.Credential{User: "deploy", Token: "glpat-web"}},
		{Name: "docs", URL: "https://git.example.com/ops/docs.git"}, // public
	}
	w.Pass = func(context.Context) (string, error) { return "v2.egress-pass", nil }

	if err := w.Ready(context.Background(), config.Persona{Nick: "kai", Name: "Kai", Team: "Engineering"}); err != nil {
		t.Fatal(err)
	}

	c := sb.commands()[0]
	args := strings.Join(c.Args, " ")
	input := c.Stdin // with what's encoded in it, decoded
	for _, f := range strings.Fields(c.Stdin) {
		if b, err := base64.StdEncoding.DecodeString(f); err == nil {
			input += "\n" + string(b)
		}
	}
	for _, secret := range []string{"glpat-infra", "glpat-web", "v2.egress-pass", "ghs_readonly"} {
		if !strings.Contains(input, secret) || strings.Contains(args, secret) {
			t.Errorf("%s went in %q / %v; want only on the input", secret, c.Stdin, c.Args)
		}
	}
	for _, want := range []string{"https://git.example.com/ops/infra.git", "/workspace/infra", "https://git.example.com/ops/docs.git", "/workspace/docs",
		// web is GitHub's here too: the other one goes beside it.
		"https://github.com/acme/web", "/workspace/web", "https://git.example.com/ops/web.git", "/workspace/git-web",
		// Commits made at the desk are the agent's.
		"Kai (kai, AI agent)", "kai@agents.guildnet.dev"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("ran %v; want %q in it", c.Args, want)
		}
	}
}

// What a desk shows its agent never has a secret the runner handed it: a
// command that prints the credentials gets them blanked out.
func TestADesksSecretsAreBlankedFromWhatItShows(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, fakeRepos{urls: []string{"https://github.com/acme/web"}, token: "ghs_readonly"})
	w.Git = fakeGit{{Name: "infra", URL: "https://git.example.com/ops/infra.git", Read: gitwire.Credential{User: "deploy", Token: "glpat-infra"}}}
	w.Pass = func(context.Context) (string, error) { return "v2.egress-pass", nil }
	w.Ready(context.Background(), kai)
	sb.answer = func(desk.Command) desk.Output {
		return desk.Output{Text: "https://deploy:glpat-infra@git.example.com/ops/infra.git\nghs_readonly\nhttp://guildnet:v2.egress-pass@proxy:3128\n"} // trufflehog:ignore (made up)
	}
	sb.Write(context.Background(), "kai", "/workspace/creds", []byte("token=glpat-infra\n"))

	ran, _ := w.Run(context.Background(), kai, "bash", map[string]any{"command": "cat ~/.config/guildnet/*"})
	read, _ := w.Run(context.Background(), kai, "read", map[string]any{"path": "creds"})
	found, _ := w.Run(context.Background(), kai, "grep", map[string]any{"pattern": "glpat"})

	for _, out := range []string{ran, read, found} {
		for _, secret := range []string{"glpat-infra", "ghs_readonly", "v2.egress-pass"} {
			if strings.Contains(out, secret) {
				t.Errorf("the agent was shown %s:\n%s", secret, out)
			}
		}
	}
	if !strings.Contains(ran, "[a secret]") {
		t.Errorf("the agent should be told something was blanked out:\n%s", ran)
	}
}

// Every command goes out with the network's egress pass as it is now, so
// whatever it runs (curl, pip, npm) reaches the hosts the owner chose, one
// added a moment ago too; the pass, printed, is blanked like any secret.
func TestEveryCommandCarriesTheNetworksPassAsItIsNow(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, nil)
	passes := []string{"v2.pass-before", "v2.pass-after"}
	w.Pass = func(context.Context) (string, error) {
		p := passes[0]
		if len(passes) > 1 {
			passes = passes[1:]
		}
		return p, nil
	}
	w.Ready(context.Background(), kai)
	sb.answer = func(c desk.Command) desk.Output {
		return desk.Output{Text: "HTTPS_PROXY=http://guildnet:" + c.Pass + "@proxy:3128\n"}
	}

	first, _ := w.Run(context.Background(), kai, "bash", map[string]any{"command": "env"})
	w.Run(context.Background(), kai, "bash", map[string]any{"command": "curl https://api.stripe.com"})

	ran := sb.commands()
	if got := ran[len(ran)-1].Pass; got != "v2.pass-after" {
		t.Errorf("the second command went with %q, want the pass as it is now", got)
	}
	if got := ran[len(ran)-2].Pass; got != "v2.pass-before" {
		t.Errorf("the first command went with %q", got)
	}
	if strings.Contains(first, "v2.pass-before") || !strings.Contains(first, "[a secret]") {
		t.Errorf("the agent was shown the pass:\n%s", first)
	}
	// Without a pass to give (no hosts beyond the usual), commands go as
	// they are.
	w.Pass = func(context.Context) (string, error) { return "", nil }
	w.Run(context.Background(), kai, "bash", map[string]any{"command": "ls"})
	if ran := sb.commands(); ran[len(ran)-1].Pass != "" {
		t.Errorf("a command went with pass %q", ran[len(ran)-1].Pass)
	}
}

// A pass is asked for once in a while, not for every command; one for
// other hosts (the owner added one) is asked for at once.
func TestAPassIsKeptAWhileForTheSameHosts(t *testing.T) {
	asked := 0
	hosts := []string{"api.stripe.com"}
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	pass := desk.KeptPass(func(_ context.Context, h []string) (string, error) {
		asked++
		return fmt.Sprintf("v2.pass-%d-%s", asked, strings.Join(h, ",")), nil
	}, func() []string { return hosts }, 4*time.Minute, func() time.Time { return now })

	first, _ := pass(context.Background())
	again, _ := pass(context.Background())
	if first != again || asked != 1 {
		t.Errorf("asked %d times for the same hosts: %q then %q", asked, first, again)
	}
	hosts = []string{"api.stripe.com", "crates.io"}
	if p, _ := pass(context.Background()); !strings.HasSuffix(p, "api.stripe.com,crates.io") {
		t.Errorf("with a host added: %q, want a new pass for both", p)
	}
	now = now.Add(5 * time.Minute)
	if pass(context.Background()); asked != 3 {
		t.Errorf("after a while, asked %d times, want a fresh pass", asked)
	}
}

func TestADeskWithoutReposIsJustWokenAndMadeFresh(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, fakeRepos{})

	if err := w.Ready(context.Background(), kai); err != nil {
		t.Fatal(err)
	}

	if len(sb.opened) != 1 || len(sb.commands()) != 1 || strings.Contains(strings.Join(sb.commands()[0].Args, " "), "clone") {
		t.Errorf("opened %v, ran %v; want only the home made fresh", sb.opened, sb.commands())
	}
}

// What starts by itself in a desk (a shell's, git's, Go's, npm's or
// Python's startup files) is put back as the image has it whenever the desk
// is readied: something an earlier wake-up was steered into leaving there
// doesn't run on every command after, collecting each fresh token. Commands
// don't read a shell's startup files at all.
func TestADeskIsMadeFreshEachTimeAndRunsNoStartupFiles(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, fakeRepos{urls: []string{"https://github.com/acme/api"}, token: "t"})

	w.Ready(context.Background(), kai)
	w.Run(context.Background(), kai, "bash", map[string]any{"command": "ls"})

	ran := sb.commands()
	script := strings.Join(ran[0].Args, " ")
	for _, f := range []string{".bash_profile", ".bashrc", ".profile", ".gitconfig", ".config/go", ".npmrc", ".config/pip", "usercustomize"} {
		if !strings.Contains(script, f) {
			t.Errorf("readying the desk leaves %s as it was:\n%s", f, script)
		}
	}
	if strings.Index(script, ".bashrc") > strings.Index(script, "git clone") {
		t.Error("the home should be made fresh before anything runs in it")
	}
	if c := ran[1].Args; !slices.Equal(c[:3], []string{"bash", "--noprofile", "--norc"}) {
		t.Errorf("a command ran as %v; want no startup files read", c)
	}
}

func TestADeskWhoseSandboxWontWakeIsntReady(t *testing.T) {
	sb := newSandbox()
	sb.openErr = errors.New("the sandbox VM is starting")
	w := desk.New(sb, fakeRepos{urls: []string{"https://github.com/acme/api"}, token: "t"})

	err := w.Ready(context.Background(), kai)

	if err == nil || !strings.Contains(err.Error(), "starting") || len(sb.commands()) != 0 {
		t.Errorf("err = %v, ran %v", err, sb.commands())
	}
}

// The sandbox VM powers off when no desk has worked for a while. A desk
// that can't reach it asks for it to be woken, once, and tries again until
// it's up.
func TestADeskWakesTheSandboxWhenItsAsleep(t *testing.T) {
	sb := &sleepy{fakeSandbox: newSandbox(), asleepFor: 3}
	w := desk.New(sb, nil)
	wakes := 0
	w.Wake = func(context.Context) error { wakes++; return nil }
	w.Retry = time.Millisecond

	err := w.Ready(context.Background(), kai)

	if err != nil || wakes != 1 || sb.tries != 4 {
		t.Errorf("err %v, woken %d times, %d tries", err, wakes, sb.tries)
	}
}

func TestADeskThatNeverWakesGivesUp(t *testing.T) {
	sb := &sleepy{fakeSandbox: newSandbox(), asleepFor: 1 << 30}
	w := desk.New(sb, nil)
	w.Wake = func(context.Context) error { return nil }
	w.Retry = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := w.Ready(ctx, kai)

	if err == nil || !strings.Contains(err.Error(), "didn't wake") {
		t.Errorf("err = %v", err)
	}
}

// The sandbox holds only so many desks at once: when every one is working,
// a desk waits its turn rather than failing.
func TestADeskWaitsItsTurnWhenTheSandboxIsFull(t *testing.T) {
	sb := &crowded{fakeSandbox: newSandbox(), fullFor: 3}
	w := desk.New(sb, nil)
	w.Retry = time.Millisecond

	err := w.Ready(context.Background(), kai)

	if err != nil || sb.tries != 4 {
		t.Errorf("err %v, %d tries", err, sb.tries)
	}
}

func TestADeskThatNeverGetsATurnSaysTheSandboxIsFull(t *testing.T) {
	sb := &crowded{fakeSandbox: newSandbox(), fullFor: 1 << 30}
	w := desk.New(sb, nil)
	w.Retry = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := w.Ready(ctx, kai)

	if !errors.Is(err, desk.ErrFull) {
		t.Errorf("err = %v", err)
	}
}

// crowded is a sandbox with no room for a desk for its first few tries.
type crowded struct {
	*fakeSandbox
	fullFor, tries int
}

func (s *crowded) Open(ctx context.Context, nick string) error {
	s.tries++
	if s.tries <= s.fullFor {
		return fmt.Errorf("the sandbox: %w", desk.ErrFull)
	}
	return s.fakeSandbox.Open(ctx, nick)
}

// sleepy is a sandbox that can't be reached for its first few tries.
type sleepy struct {
	*fakeSandbox
	asleepFor, tries int
}

func (s *sleepy) Open(ctx context.Context, nick string) error {
	s.tries++
	if s.tries <= s.asleepFor {
		return fmt.Errorf("dialing: %w", desk.ErrUnreachable)
	}
	return s.fakeSandbox.Open(ctx, nick)
}

// Firing an agent while the sandbox VM sleeps: the VM is woken and the desk
// removed once it's up (firing doesn't wait the minute that takes), so a
// new agent given the nick doesn't find the old one's work.
func TestADeskIsRemovedOnceTheSandboxWakes(t *testing.T) {
	sb := &asleepToRemove{fakeSandbox: newSandbox(), asleepFor: 3}
	w := desk.New(sb, nil)
	var mu sync.Mutex
	wakes := 0
	w.Wake = func(context.Context) error { mu.Lock(); wakes++; mu.Unlock(); return nil }
	w.Retry = time.Millisecond

	if err := w.Clear(context.Background(), kai); err != nil {
		t.Fatal(err)
	}

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		sb.mu.Lock()
		done := slices.Equal(sb.removed, []string{"kai"})
		sb.mu.Unlock()
		if done {
			break
		}
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sb.removed, []string{"kai"}) || wakes != 1 {
		t.Errorf("removed %v, woken %d times", sb.removed, wakes)
	}
}

// asleepToRemove is a sandbox that can't be reached for its first few
// removals.
type asleepToRemove struct {
	*fakeSandbox
	asleepFor, tries int
}

func (s *asleepToRemove) Remove(ctx context.Context, nick string) error {
	s.mu.Lock()
	s.tries++
	asleep := s.tries <= s.asleepFor
	s.mu.Unlock()
	if asleep {
		return fmt.Errorf("dialing: %w", desk.ErrUnreachable)
	}
	return s.fakeSandbox.Remove(ctx, nick)
}

func TestACheckoutThatFailsSaysWhy(t *testing.T) {
	sb := newSandbox()
	sb.answer = func(desk.Command) desk.Output { return desk.Output{Text: "fatal: repository not found", Exit: 128} }
	w := desk.New(sb, fakeRepos{urls: []string{"https://github.com/acme/api"}, token: "t"})

	err := w.Ready(context.Background(), kai)

	if err == nil || !strings.Contains(err.Error(), "repository not found") {
		t.Errorf("err = %v", err)
	}
}

func TestFiringAnAgentRemovesItsDesk(t *testing.T) {
	sb := newSandbox()
	w := desk.New(sb, nil)

	if err := w.Clear(context.Background(), kai); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(sb.removed, []string{"kai"}) {
		t.Errorf("removed %v", sb.removed)
	}
}
