// SPDX-License-Identifier: AGPL-3.0-only

package gitremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
)

// Pusher is git_push_branch: an agent at a desk pushes a branch of its work
// to one of its team's git repositories. The desk makes a pack of the
// branch's new commits; the runner pushes it with the repository's push
// credential, from the broker for this push alone, so the credential never
// reaches a desk. A push moves only the agent's own branch (<nick>/<topic>),
// never the default branch, and only from where the host has it, never over
// commits the desk doesn't have. People merge on the host.
type Pusher struct {
	Live      *Live
	TeamRepos func(team string) []string
	// Desk is where agents' desks are (sandboxd).
	Desk desk.Sandbox
	// HTTP reaches git hosts (egress: public addresses, and the private
	// hosts whoever runs the install named).
	HTTP *http.Client
	// Pushed goes in the org log; nil for nowhere.
	Pushed func(p config.Persona, repo, branch, commit string)
	// MaxPack is the most a push may carry; none means 8 MB.
	MaxPack int64
}

const pushTool = "git_push_branch"

// packPath is where the desk leaves the pack, for the runner to read.
const packPath = "/tmp/guildnet-push.pack"

// pushable is p's team's repositories it may push to.
func (u *Pusher) pushable(p config.Persona) []Repo {
	repos := u.Live.Repos()
	var out []Repo
	for _, name := range u.TeamRepos(p.Team) {
		name, ok := strings.CutPrefix(name, "git:")
		if !ok {
			continue
		}
		if r, ok := Find(repos, name); ok && r.Write {
			out = append(out, r)
		}
	}
	return out
}

func names(repos []Repo) string {
	var out []string
	for _, r := range repos {
		out = append(out, r.Name)
	}
	return strings.Join(out, ", ")
}

// Tools is git_push_branch, for an agent whose team has a repository it may
// push to.
func (u *Pusher) Tools(p config.Persona) []agent.Tool {
	repos := u.pushable(p)
	if len(repos) == 0 {
		return nil
	}
	return []agent.Tool{{Name: pushTool,
		Description: "Push a branch of your work at your desk to one of your team's git repositories (" + names(repos) + "; GitHub's take a pull request instead). " +
			"Commit on a local branch named " + strings.ToLower(p.Nick) + "/<topic> in the repository's folder, then push it: people merge it on the host.",
		Properties: map[string]any{
			"repo":   map[string]any{"type": "string", "description": "The repository: " + names(repos)},
			"branch": map[string]any{"type": "string", "description": "Your branch, " + strings.ToLower(p.Nick) + "/<topic>: the same name at your desk and on the host"},
		},
		Required: []string{"repo", "branch"}}}
}

// Guide says where the repositories are at the desk.
func (u *Pusher) Guide(p config.Persona) string {
	repos := u.pushable(p)
	if len(repos) == 0 {
		return ""
	}
	return "Your team's git repositories outside GitHub (" + names(repos) + ") are in /workspace/<name> at your desk. To hand work on one over, commit it on a branch " +
		strings.ToLower(p.Nick) + "/<topic> and push it with " + pushTool + "; if the branch moved on the host, fetch and rebase first."
}

// Prepare is never needed: a push doesn't ask a person.
func (u *Pusher) Prepare(_ context.Context, _ config.Persona, name string, _ map[string]any) (string, map[string]any, error) {
	return "", nil, fmt.Errorf("%s doesn't need approval", name)
}

// SourceOf is the team's code, as a desk's reads are: pushing work from the
// desk doesn't ask.
func (u *Pusher) SourceOf(string) string { return agent.YourRepos }

// Step describes a push for IRC.
func (u *Pusher) Step(name string, in map[string]any) string {
	if name != pushTool {
		return ""
	}
	return fmt.Sprintf("pushes %v to %v", in["branch"], in["repo"])
}

// packScript runs at the desk, in the repository's checkout (whichever of
// its folders has the repository as its origin): it checks the branch is
// there and has whatever the host has of it, then packs what the branch has
// that the host's refs (on its input, after the branch's own) don't, and
// says the branch's commit.
const packScript = `set -eu
name=$1 url=$2 branch=$3 out=$4
dir=
for d in "/workspace/$name" "/workspace/git-$name"; do
  if [ -d "$d/.git" ] && [ "$(git -C "$d" config --get remote.origin.url || true)" = "$url" ]; then dir=$d; break; fi
done
if [ -z "$dir" ]; then echo "there's no checkout of $name at your desk"; exit 3; fi
cd "$dir"
if ! tip=$(git rev-parse --verify --quiet "refs/heads/$branch^{commit}"); then echo "there's no branch $branch in $dir: commit your work on it first"; exit 4; fi
read -r old || old=
if [ -n "$old" ] && ! { git cat-file -e "$old^{commit}" 2>/dev/null && git merge-base --is-ancestor "$old" "$tip"; }; then
  echo "$branch on the host has commits yours doesn't: fetch, rebase onto origin/$branch, and push again"; exit 5
fi
{ echo "$tip"; while read -r oid; do if git cat-file -e "$oid^{commit}" 2>/dev/null; then echo "^$oid"; fi; done; } | git pack-objects --revs --stdout -q > "$out"
echo "$tip"`

// Run pushes the branch.
func (u *Pusher) Run(ctx context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	if name != pushTool {
		return "", fmt.Errorf("there's no tool called %s", name)
	}
	repoName, _ := in["repo"].(string)
	branch, _ := in["branch"].(string)
	repoName = strings.TrimPrefix(strings.TrimSpace(repoName), "git:")
	branch = strings.TrimSpace(branch)
	repos := u.pushable(p)
	repo, ok := Find(repos, repoName)
	if !ok {
		return "", fmt.Errorf("%q isn't one of your team's git repositories you can push to: %s", repoName, names(repos))
	}
	nick := strings.ToLower(p.Nick)
	ref := "refs/heads/" + branch
	if topic, ok := strings.CutPrefix(branch, nick+"/"); !ok || topic == "" || !gitwire.BranchRef(ref) {
		return "", fmt.Errorf("push to a branch of your own, %s/<topic>, not %q", nick, branch)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	addr, cred, err := u.Live.Broker.Credential(ctx, repo.Name, true)
	if err != nil {
		return "", fmt.Errorf("couldn't get %s's push credential: %w", repo.Name, err)
	}
	// Where the branch is on the host, and which is the default branch.
	refs, err := gitwire.List(ctx, u.HTTP, addr, cred, gitwire.Pushing)
	if err != nil {
		return "", fmt.Errorf("%s: %w", repo.Name, err)
	}
	if fetch, err := gitwire.List(ctx, u.HTTP, addr, cred, gitwire.Fetching); err == nil && fetch.Head == ref {
		return "", fmt.Errorf("%s is %s's default branch: push to a branch of your own, and people merge it", branch, repo.Name)
	}
	old := refs.Refs[ref]
	var have strings.Builder
	have.WriteString(old + "\n")
	for _, oid := range refs.Refs {
		have.WriteString(oid + "\n")
	}
	out, err := u.Desk.Run(ctx, p.Nick, desk.Command{Args: []string{"bash", "--noprofile", "--norc", "-c", packScript, "pack", repo.Name, addr, branch, packPath},
		Dir: desk.Root, Stdin: have.String(), Timeout: 2 * time.Minute})
	if err != nil {
		return "", fmt.Errorf("packing at your desk: %w", err)
	}
	if out.Exit != 0 || out.TimedOut {
		return "", errors.New(strings.TrimSpace(lastLine(out.Text)))
	}
	tip := strings.TrimSpace(lastLine(out.Text))
	most := u.MaxPack
	if most <= 0 {
		most = 8 << 20
	}
	pack, err := u.Desk.Read(ctx, p.Nick, packPath, most)
	u.Desk.Run(ctx, p.Nick, desk.Command{Args: []string{"rm", "-f", packPath}, Dir: desk.Root, Timeout: time.Minute})
	if errors.Is(err, desk.ErrTooBig) {
		return "", fmt.Errorf("the push is too big (over %d MB): push fewer or smaller changes at a time", most>>20)
	}
	if err != nil {
		return "", fmt.Errorf("reading the pack from your desk: %w", err)
	}
	res, err := gitwire.Push(ctx, u.HTTP, addr, cred, gitwire.Update{Ref: ref, Old: old, New: tip}, bytes.NewReader(pack))
	var rejected *gitwire.RejectedError
	switch {
	case errors.Is(err, gitwire.ErrMoved):
		return "", fmt.Errorf("%s moved on the host while you pushed: fetch, rebase onto origin/%s, and push again", branch, branch)
	case errors.As(err, &rejected):
		msg := fmt.Sprintf("%s refused the push: %s", repo.Name, rejected.Reason)
		if rejected.Said != "" {
			msg += "\n" + agent.Untrusted("git host", rejected.Said)
		}
		return "", errors.New(msg)
	case err != nil:
		return "", fmt.Errorf("pushing to %s: %w", repo.Name, err)
	}
	if u.Pushed != nil {
		u.Pushed(p, repo.Name, branch, tip)
	}
	said := fmt.Sprintf("Pushed %s to %s, at %s. People merge it on the host.", branch, repo.Name, tip[:7])
	if res.Said != "" {
		said += "\nThe host said:\n" + agent.Untrusted("git host", res.Said)
	}
	return said, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

var _ agent.Toolbox = (*Pusher)(nil)
