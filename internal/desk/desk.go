// SPDX-License-Identifier: AGPL-3.0-only

// Package desk is desks of GuildNet's own: each agent at one has a
// workspace in a sandbox (see sandboxd), and works in it through six tools
// the runner carries out there: bash, read, write, edit, glob and grep. The
// loop that calls the model is the runner's own (package brain), so desks
// work on any model; keys never enter the sandbox.
package desk

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Root is where an agent's workspace is, in its sandbox.
const Root = "/workspace"

const (
	// defaultTimeout and maxTimeout are how long a command may run.
	defaultTimeout = 2 * time.Minute
	maxTimeout     = 10 * time.Minute
	// maxFile is the largest file read and edit take.
	maxFile = 2 << 20
	// readLines is how many lines read shows unless asked for fewer.
	readLines = 2000
	// maxLine is where a very long line is cut, for read.
	maxLine = 2000
	// maxListed is how many files or matches a search shows.
	maxListed = 300
)

var (
	// ErrNoFile is a sandbox's answer for a file that isn't there.
	ErrNoFile = errors.New("no such file")
	// ErrTooBig is a sandbox's answer for a file larger than asked for.
	ErrTooBig = errors.New("file too big")
	// ErrUnreachable is a sandbox that doesn't answer: the VM may be asleep.
	ErrUnreachable = errors.New("the sandbox can't be reached")
	// ErrFull is a sandbox holding all the desks it has room for, every one
	// at work: try again shortly.
	ErrFull = errors.New("the sandbox is full: every desk it has room for is working")
)

// Command is something to run in a desk's sandbox.
type Command struct {
	Args    []string      // the program and its arguments; no shell unless asked for
	Dir     string        // where it runs
	Stdin   string        // its input (secrets go here, never in Args)
	Timeout time.Duration // it's stopped after this
	// Pass is the network's egress pass, for the desks' proxy to open its
	// hosts to whatever the command reaches; "" for none.
	Pass string
}

// Output is what came of a command: what it printed (stdout and stderr
// together, cut short past a limit), and how it ended.
type Output struct {
	Text     string
	Exit     int
	TimedOut bool
}

// Sandbox is where desks are: one workspace per agent, by nick, reached
// through sandboxd (tests use a stand-in).
type Sandbox interface {
	// Open makes the agent's desk ready to use, making it or waking it as
	// needed.
	Open(ctx context.Context, nick string) error
	Run(ctx context.Context, nick string, c Command) (Output, error)
	// Read gives a file's content: ErrNoFile if it isn't there, ErrTooBig if
	// it's larger than max.
	Read(ctx context.Context, nick, path string, max int64) ([]byte, error)
	// Write makes or replaces a file, and the directories it's in.
	Write(ctx context.Context, nick, path string, data []byte) error
	// Remove deletes the agent's desk and its workspace.
	Remove(ctx context.Context, nick string) error
}

// Repos is what an agent's workspace checks out: its team's repos, and a
// read-only token for them (github.Client).
type Repos interface {
	Checkout(ctx context.Context, p config.Persona) (urls []string, token string, err error)
}

// Clone is a git repository on any host (not GitHub's app) a desk checks
// out: its name, where it is, and the credential to read it with (none for
// a public one). The credential reaches the desk, so it should only read.
type Clone struct {
	Name, URL string
	Read      gitwire.Credential
}

// GitRepos is the git repositories on any host an agent's team works on
// (gitremote).
type GitRepos interface {
	Clones(ctx context.Context, p config.Persona) ([]Clone, error)
}

// Workspace is the tools an agent at a desk works in its workspace with. It
// is an agent.Toolbox.
type Workspace struct {
	sb    Sandbox
	repos Repos
	// Git is the team's git repositories on other hosts; nil for none.
	Git GitRepos
	// Pass is the network's egress pass, which lets its desks reach its git
	// repositories' hosts, and the hosts its owner chose, through the desks'
	// proxy: asked for afresh for each command; nil for none.
	Pass func(ctx context.Context) (string, error)
	// Wake asks for the sandbox VM to be started, when it can't be reached
	// (it powers off when idle); nil if it never sleeps.
	Wake func(ctx context.Context) error
	// Retry is how often a woken sandbox is tried again.
	Retry time.Duration

	mu sync.Mutex
	// secrets are what each desk was handed when last readied (tokens, the
	// pass), and passes the latest passes its commands went out with: both
	// blanked out of whatever it shows its agent.
	secrets, passes map[string][]string
}

// New is the workspace tools, in sb, checking out repos (nil for none).
func New(sb Sandbox, repos Repos) *Workspace {
	return &Workspace{sb: sb, repos: repos}
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

// Tools are the same for every agent. None asks a person: they only reach
// the agent's own workspace. What they return is for the agent; the step
// is what IRC is shown.
func (w *Workspace) Tools(config.Persona) []agent.Tool {
	file := prop("string", "Path to the file, from /workspace or absolute")
	dir := prop("string", "Directory to search, from /workspace or absolute; defaults to /workspace")
	return []agent.Tool{
		{Name: "bash", ReadOnly: true,
			Description: "Run a shell command in your workspace (bash, starting in /workspace). Each command starts afresh: cd and variables don't carry over, files do. Output over 64 KB is cut.",
			Properties: map[string]any{
				"command": prop("string", "The command"),
				"timeout": prop("integer", "Seconds it may run: 120 unless you say, at most 600"),
			},
			Required: []string{"command"}},
		{Name: "read", ReadOnly: true,
			Description: "Read a file in your workspace, with line numbers. Long files: ask for a range of lines.",
			Properties: map[string]any{
				"path":   file,
				"offset": prop("integer", "Line to start at (1 is the first)"),
				"limit":  prop("integer", "How many lines; 2000 unless you say"),
			},
			Required: []string{"path"}},
		{Name: "write", ReadOnly: true,
			Description: "Write a file in your workspace, replacing it if it's there. For a change to an existing file, use edit.",
			Properties:  map[string]any{"path": file, "content": prop("string", "The file's whole content")},
			Required:    []string{"path", "content"}},
		{Name: "edit", ReadOnly: true,
			Description: "Change a file in your workspace: replace exact text, which must be in exactly one place unless replace_all is set.",
			Properties: map[string]any{
				"path":        file,
				"old":         prop("string", "Exact text in the file, including whitespace"),
				"new":         prop("string", "What replaces it"),
				"replace_all": prop("boolean", "Replace every place it's in"),
			},
			Required: []string{"path", "old", "new"}},
		{Name: "glob", ReadOnly: true,
			Description: "Find files in your workspace by name, e.g. **/*.go.",
			Properties:  map[string]any{"pattern": prop("string", "Glob pattern"), "path": dir},
			Required:    []string{"pattern"}},
		{Name: "grep", ReadOnly: true,
			Description: "Search file contents in your workspace with a regular expression (ripgrep). Shows file:line: text.",
			Properties: map[string]any{
				"pattern":     prop("string", "Regular expression"),
				"path":        dir,
				"glob":        prop("string", "Only files matching this, e.g. *.go"),
				"ignore_case": prop("boolean", "Ignore case"),
			},
			Required: []string{"pattern"}},
	}
}

// Guide says nothing more: what an agent at a desk is told about its
// workspace is in its system prompt (agent.Desk).
func (w *Workspace) Guide(config.Persona) string { return "" }

// Prepare is never needed: no workspace tool asks a person.
func (w *Workspace) Prepare(_ context.Context, _ config.Persona, name string, _ map[string]any) (string, map[string]any, error) {
	return "", nil, fmt.Errorf("%s doesn't need approval", name)
}

// SourceOf is where what a desk reads comes from (agent.Sourced): the
// team's repos, and the hosts it may reach, so it counts as the team's
// code. What it read can't steer a write to another service without a
// person seeing it first; a pull request or a pushed branch of its work is
// what it's for.
func (w *Workspace) SourceOf(string) string { return agent.YourRepos }

// Run carries out a call in the agent's workspace. What it read or ran is
// marked untrusted: it's whatever is in the repos and on the web.
func (w *Workspace) Run(ctx context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	switch name {
	case "bash", "read", "glob", "grep":
		out, err := w.look(ctx, p, name, in)
		if err != nil {
			return "", errors.New(w.blank(p.Nick, err.Error()) + outOfSpace(err.Error()))
		}
		return agent.Untrusted("desk", w.blank(p.Nick, out)) + outOfSpace(out), nil
	}
	out, err := w.change(ctx, p, name, in)
	if err != nil {
		return "", errors.New(err.Error() + outOfSpace(err.Error()))
	}
	return out, nil
}

// outOfSpace is what the agent is told, after what its desk said, if that
// says it ran out of space: the network's desks' storage is full (on XFS a
// network's quota says either).
func outOfSpace(said string) string {
	if !strings.Contains(said, "No space left on device") && !strings.Contains(said, "Disk quota exceeded") {
		return ""
	}
	return "\n[Your network's desks' storage is full: nothing more can be saved at any desk until some is freed. Delete files you've finished with (du -sh * shows what's big), and tell the network's owner, who can clear a desk that isn't needed from its agent's page in the console.]"
}

// blank is s without the secrets p's desk was handed.
func (w *Workspace) blank(nick, s string) string {
	w.mu.Lock()
	secrets := append(slices.Clone(w.passes[strings.ToLower(nick)]), w.secrets[strings.ToLower(nick)]...)
	w.mu.Unlock()
	// Longest first, so one inside another is blanked whole.
	slices.SortStableFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, "[a secret]")
	}
	return s
}

// handed notes the secrets nick's desk was just handed: short ones aren't
// worth blanking (they'd blank ordinary words).
func (w *Workspace) handed(nick string, secrets []string) {
	var keep []string
	for _, s := range secrets {
		if len(s) >= 8 {
			keep = append(keep, s)
		}
	}
	// Longest first, so one inside another is blanked whole.
	slices.SortFunc(keep, func(a, b string) int { return len(b) - len(a) })
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.secrets == nil {
		w.secrets = map[string][]string{}
	}
	w.secrets[strings.ToLower(nick)] = keep
}

// passFor is the network's egress pass as it is now, for a command at
// nick's desk (and blanked from what it shows), or "" for none. A pass
// that can't be had leaves the command to the hosts every desk reaches.
func (w *Workspace) passFor(ctx context.Context, nick string) string {
	if w.Pass == nil {
		return ""
	}
	pass, err := w.Pass(ctx)
	if err != nil || pass == "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.passes == nil {
		w.passes = map[string][]string{}
	}
	k := strings.ToLower(nick)
	if !slices.Contains(w.passes[k], pass) {
		w.passes[k] = append(w.passes[k], pass)
		// A command's background work outlives it a little: the last few.
		if n := len(w.passes[k]); n > keptPasses {
			w.passes[k] = w.passes[k][n-keptPasses:]
		}
	}
	return pass
}

// keptPasses is how many of a desk's latest passes are blanked from what it
// shows (each command is given one freshly signed).
const keptPasses = 8

func (w *Workspace) look(ctx context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	switch name {
	case "bash":
		return w.bash(ctx, p, in)
	case "read":
		return w.read(ctx, p, in)
	case "glob":
		return w.search(ctx, p, []string{"rg", "--files", "--hidden", "--glob", "!.git", "--glob", str(in, "pattern"), where(str(in, "path"))}, "No files match.")
	}
	args := []string{"rg", "--line-number", "--no-heading", "--color=never", "--hidden", "--glob", "!.git", "--max-columns=300"}
	if b, _ := in["ignore_case"].(bool); b {
		args = append(args, "-i")
	}
	if g := str(in, "glob"); g != "" {
		args = append(args, "--glob", g)
	}
	return w.search(ctx, p, append(args, "--", str(in, "pattern"), where(str(in, "path"))), "No matches.")
}

func (w *Workspace) change(ctx context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	switch name {
	case "write":
		f := where(str(in, "path"))
		if err := w.sb.Write(ctx, p.Nick, f, []byte(str(in, "content"))); err != nil {
			return "", err
		}
		return fmt.Sprintf("Wrote %s (%d bytes).", f, len(str(in, "content"))), nil
	case "edit":
		return w.edit(ctx, p, in)
	}
	return "", fmt.Errorf("there's no tool called %s", name)
}

func (w *Workspace) bash(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	cmd := str(in, "command")
	if strings.TrimSpace(cmd) == "" {
		return "", errors.New("give a command to run")
	}
	timeout := defaultTimeout
	if s := num(in, "timeout"); s > 0 {
		timeout = min(time.Duration(s)*time.Second, maxTimeout)
	}
	out, err := w.sb.Run(ctx, p.Nick, Command{Args: shell(cmd), Dir: Root, Timeout: timeout, Pass: w.passFor(ctx, p.Nick)})
	if err != nil {
		return "", err
	}
	text := out.Text
	switch {
	case out.TimedOut:
		text += fmt.Sprintf("\n[stopped after %s: it took too long. Split the work up, or ask for a longer timeout (at most 600 seconds).]", timeout)
	case out.Exit != 0:
		text += fmt.Sprintf("\n[exit status %d]", out.Exit)
	case text == "":
		text = "(no output)"
	}
	return text, nil
}

func (w *Workspace) read(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	f := where(str(in, "path"))
	data, err := w.sb.Read(ctx, p.Nick, f, maxFile)
	if errors.Is(err, ErrTooBig) {
		return "", fmt.Errorf("%s is over 2 MB: look at part of it with grep, or head, tail or sed in bash", f)
	}
	if err != nil {
		return "", err
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	from := max(num(in, "offset"), 1)
	n := num(in, "limit")
	if n <= 0 {
		n = readLines
	}
	var b strings.Builder
	for i := from; i < from+n && i <= len(lines); i++ {
		line := strings.TrimRight(lines[i-1], "\n")
		if len(line) > maxLine {
			line = line[:maxLine] + " [line cut short]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, line)
	}
	if from+n <= len(lines) {
		fmt.Fprintf(&b, "[%d more lines: read from line %d for more]\n", len(lines)-(from+n-1), from+n)
	}
	if b.Len() == 0 {
		return fmt.Sprintf("%s has %d lines, none from line %d.", f, len(lines), from), nil
	}
	return b.String(), nil
}

func (w *Workspace) edit(ctx context.Context, p config.Persona, in map[string]any) (string, error) {
	f, old := where(str(in, "path")), str(in, "old")
	if old == "" {
		return "", errors.New("old is empty: give the exact text to replace")
	}
	data, err := w.sb.Read(ctx, p.Nick, f, maxFile)
	if errors.Is(err, ErrTooBig) {
		return "", fmt.Errorf("%s is over 2 MB: change it from bash instead", f)
	}
	if err != nil {
		return "", err
	}
	text := string(data)
	all, _ := in["replace_all"].(bool)
	switch n := strings.Count(text, old); {
	case n == 0:
		return "", fmt.Errorf("that text isn't in %s: read it again and copy the text exactly", f)
	case n > 1 && !all:
		return "", fmt.Errorf("that text is in %d places in %s: give more of it so it's in one, or set replace_all", n, f)
	}
	text = strings.ReplaceAll(text, old, str(in, "new"))
	if err := w.sb.Write(ctx, p.Nick, f, []byte(text)); err != nil {
		return "", err
	}
	return "Edited " + f + ".", nil
}

// search runs ripgrep, which exits 1 when it finds nothing.
func (w *Workspace) search(ctx context.Context, p config.Persona, args []string, none string) (string, error) {
	out, err := w.sb.Run(ctx, p.Nick, Command{Args: args, Dir: Root, Timeout: defaultTimeout})
	if err != nil {
		return "", err
	}
	switch {
	case out.TimedOut:
		return "", errors.New("the search took too long: narrow it to a directory or a kind of file")
	case out.Exit == 1 && strings.TrimSpace(out.Text) == "":
		return none, nil
	case out.Exit != 0:
		return "", fmt.Errorf("the search failed: %s", strings.TrimSpace(out.Text))
	}
	lines := strings.Split(strings.TrimRight(out.Text, "\n"), "\n")
	if args[1] == "--files" {
		slices.Sort(lines)
	}
	if len(lines) > maxListed {
		more := len(lines) - maxListed
		lines = append(lines[:maxListed], fmt.Sprintf("[%d more: narrow the search]", more))
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// Step describes a call for IRC.
func (w *Workspace) Step(name string, in map[string]any) string {
	switch name {
	case "bash":
		return "runs " + short(str(in, "command"), 120)
	case "read":
		return "reads " + str(in, "path")
	case "write":
		return "writes " + str(in, "path")
	case "edit":
		return "edits " + str(in, "path")
	case "glob":
		return "looks for files matching " + short(str(in, "pattern"), 60)
	case "grep":
		return "searches files for " + short(str(in, "pattern"), 60)
	}
	return ""
}

// fresh is run in the sandbox whenever a desk is readied, before anything
// else: what starts by itself there (a shell's startup files, git's, Go's,
// npm's and pip's settings, Python's own startup hooks) is put back as the
// image has it, so nothing an earlier wake-up was steered into leaving
// there runs on every command after. Caches (Go modules, npm's, pip's) stay.
// Commits made here are the agent's: its name and address are the first two
// arguments.
const fresh = `set -eu
cd "$HOME"
rm -rf .bash_profile .bash_login .profile .bashrc .bash_logout .inputrc .gitconfig .config/git .config/go .npmrc .config/pip .pip .pydistutils.cfg .netrc .config/guildnet
rm -f .local/lib/python3*/site-packages/usercustomize.py .local/lib/python3*/site-packages/sitecustomize.py .local/lib/python3*/site-packages/*.pth
git config --global init.defaultBranch main
git config --global user.name "$1"
git config --global user.email "$2"
shift 2
cd /workspace
`

// checkout is run in the sandbox to ready an agent's repos. What's secret
// comes in on its input, a line each, and is kept where git's credential
// helpers read it (each readying brings fresh ones): "github <token>",
// "git <name> <base64 of what git's helper answers>", and "pass <egress
// pass>", which git hands the desks' proxy for the git repositories' hosts.
// Its arguments are the repos, three each: github or git (and its name),
// the directory and the URL. Repos not yet there are cloned.
const checkout = `umask 077
mkdir -p "$HOME/.config/guildnet/git"
pass=
while IFS= read -r line; do
  case "$line" in
    "github "*) printf '%s' "${line#github }" > "$HOME/.config/guildnet/github-token" ;;
    "git "*) rest=${line#git }; printf '%s' "${rest#* }" | base64 -d > "$HOME/.config/guildnet/git/${rest%% *}" ;;
    "pass "*) pass=${line#pass } ;;
  esac
done
if [ -f "$HOME/.config/guildnet/github-token" ]; then
  git config --global credential.https://github.com.helper '!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=$(cat "$HOME/.config/guildnet/github-token")"; }; f'
fi
while [ $# -gt 0 ]; do
  kind=$1 dir=$2 url=$3
  shift 3
  case "$kind" in git:*)
    name=${kind#git:}
    if [ -f "$HOME/.config/guildnet/git/$name" ]; then
      git config --global "credential.$url.helper" "!f() { test \"\$1\" = get || exit 0; cat \"\$HOME/.config/guildnet/git/$name\"; }; f"
    fi
    if [ -n "$pass" ] && [ -n "${HTTPS_PROXY:-}" ]; then
      git config --global "http.$url.proxy" "http://guildnet:$pass@${HTTPS_PROXY#*://}"
      git config --global "http.$url.proxyAuthMethod" basic
    fi ;;
  esac
  if [ ! -d "$dir/.git" ]; then git clone --quiet "$url" "$dir"; fi
done`

// wakeFor is how long a woken sandbox VM gets to start.
const wakeFor = 4 * time.Minute

// waitFor is how long a desk waits its turn in a full sandbox.
const waitFor = 10 * time.Minute

// open opens p's desk, waking the sandbox VM first if it's asleep, or
// waiting its turn if it's full.
func (w *Workspace) open(ctx context.Context, p config.Persona) error {
	err := w.sb.Open(ctx, p.Nick)
	if errors.Is(err, ErrFull) {
		return w.waitTurn(ctx, p)
	}
	if !errors.Is(err, ErrUnreachable) || w.Wake == nil {
		return err
	}
	if err := w.Wake(ctx); err != nil {
		return fmt.Errorf("waking the sandbox: %w", err)
	}
	retry := w.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	deadline := time.NewTimer(wakeFor)
	defer deadline.Stop()
	tick := time.NewTicker(retry)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the sandbox didn't wake in time: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("the sandbox didn't wake within %s", wakeFor)
		case <-tick.C:
		}
		if err = w.sb.Open(ctx, p.Nick); errors.Is(err, ErrFull) {
			return w.waitTurn(ctx, p)
		} else if !errors.Is(err, ErrUnreachable) {
			return err
		}
	}
}

// A wake-up that waits for room at the sandbox: whoever's counting its desk
// time leaves the wait out, and can say it's waiting.

type waitingKey struct{}

type waiting struct {
	mu     sync.Mutex
	waited time.Duration
	onWait func() // the first time it waits
}

// CountWaiting is ctx keeping how long a desk waited for room at the
// sandbox (waited says so far), calling onWait (if not nil) when it starts
// waiting.
func CountWaiting(ctx context.Context, onWait func()) (_ context.Context, waited func() time.Duration) {
	wt := &waiting{onWait: onWait}
	return context.WithValue(ctx, waitingKey{}, wt), func() time.Duration {
		wt.mu.Lock()
		defer wt.mu.Unlock()
		return wt.waited
	}
}

// waitTurn tries p's desk again every so often until the sandbox has room
// for it, or waitFor passes.
func (w *Workspace) waitTurn(ctx context.Context, p config.Persona) error {
	if wt, ok := ctx.Value(waitingKey{}).(*waiting); ok {
		start := time.Now()
		wt.mu.Lock()
		if wt.onWait != nil {
			wt.onWait()
			wt.onWait = nil
		}
		wt.mu.Unlock()
		defer func() {
			wt.mu.Lock()
			wt.waited += time.Since(start)
			wt.mu.Unlock()
		}()
	}
	retry := w.Retry
	if retry <= 0 {
		retry = 15 * time.Second
	}
	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()
	tick := time.NewTicker(retry)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waited for room at the sandbox: %w (%w)", ErrFull, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("waited %s for room at the sandbox: %w", waitFor, ErrFull)
		case <-tick.C:
		}
		if err := w.sb.Open(ctx, p.Nick); !errors.Is(err, ErrFull) {
			return err
		}
	}
}

// Ready wakes p's desk, makes its home fresh, and checks out its team's
// repos, with a fresh read-only token.
func (w *Workspace) Ready(ctx context.Context, p config.Persona) error {
	if err := w.open(ctx, p); err != nil {
		return err
	}
	var urls []string
	var token string
	if w.repos != nil {
		var err error
		if urls, token, err = w.repos.Checkout(ctx, p); err != nil {
			return err
		}
	}
	var clones []Clone
	if w.Git != nil {
		var err error
		if clones, err = w.Git.Clones(ctx, p); err != nil {
			return err
		}
	}
	name := p.Name
	if name == "" {
		name = p.Nick
	}
	args := []string{"ready", fmt.Sprintf("%s (%s, AI agent)", name, p.Nick), strings.ToLower(p.Nick) + "@agents.guildnet.dev"}
	var stdin strings.Builder
	var secrets []string
	if token != "" {
		fmt.Fprintf(&stdin, "github %s\n", token)
		secrets = append(secrets, token)
	}
	taken := map[string]bool{}
	for _, u := range urls {
		dir := Root + "/" + path.Base(u)
		taken[dir] = true
		args = append(args, "github", dir, u)
	}
	for _, c := range clones {
		dir := Root + "/" + c.Name
		if taken[dir] {
			dir = Root + "/git-" + c.Name
		}
		args = append(args, "git:"+c.Name, dir, c.URL)
		if c.Read != (gitwire.Credential{}) {
			answer := "username=" + c.Read.User + "\npassword=" + c.Read.Token + "\n"
			fmt.Fprintf(&stdin, "git %s %s\n", c.Name, base64.StdEncoding.EncodeToString([]byte(answer)))
			secrets = append(secrets, c.Read.Token)
		}
	}
	if len(clones) > 0 && w.Pass != nil {
		pass, err := w.Pass(ctx)
		if err != nil {
			return fmt.Errorf("the network's egress pass: %w", err)
		}
		fmt.Fprintf(&stdin, "pass %s\n", pass)
		secrets = append(secrets, pass)
	}
	w.handed(p.Nick, secrets)
	script := fresh
	if len(urls)+len(clones) > 0 {
		script += checkout
	}
	out, err := w.sb.Run(ctx, p.Nick, Command{Args: append(shell(script), args...),
		Dir: Root, Stdin: stdin.String(), Timeout: maxTimeout})
	switch {
	case err != nil:
		return err
	case out.TimedOut:
		return errors.New("readying the desk took too long")
	case out.Exit != 0:
		return fmt.Errorf("readying the desk: %s", strings.TrimSpace(out.Text))
	}
	return nil
}

// shell runs script in bash, which reads no startup files: what an agent's
// desk runs is what was asked for, and only that.
func shell(script string) []string {
	return []string{"bash", "--noprofile", "--norc", "-c", script}
}

// Clear removes p's desk, workspace and all. With the sandbox VM asleep,
// it's woken and the desk removed once it's up, in the background: whoever
// fired the agent doesn't wait the minute that takes, and a new agent given
// the nick doesn't find the old one's work.
func (w *Workspace) Clear(ctx context.Context, p config.Persona) error {
	err := w.sb.Remove(ctx, p.Nick)
	if !errors.Is(err, ErrUnreachable) || w.Wake == nil {
		return err
	}
	go w.clearOnceAwake(p.Nick)
	return nil
}

func (w *Workspace) clearOnceAwake(nick string) {
	defer monitor.Recover()
	ctx, cancel := context.WithTimeout(context.Background(), wakeFor)
	defer cancel()
	err := w.Wake(ctx)
	retry := w.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for err == nil {
		if err = w.sb.Remove(ctx, nick); !errors.Is(err, ErrUnreachable) {
			break
		}
		select {
		case <-ctx.Done():
			err = fmt.Errorf("the sandbox didn't wake within %s", wakeFor)
		case <-time.After(retry):
			err = nil
		}
	}
	if err != nil {
		monitor.Problem(log.Default(), "removing %s's desk: %v", nick, err)
	}
}

// where is a path in the workspace: relative ones are from its root.
func where(p string) string {
	if p == "" {
		return Root
	}
	if !path.IsAbs(p) {
		p = path.Join(Root, p)
	}
	return path.Clean(p)
}

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}

func num(in map[string]any, k string) int {
	switch v := in[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
