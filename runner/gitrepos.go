// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/gitremote"
)

// Git repositories on any host (GitLab, Gitea, a company's own server),
// which the network's owner adds in the console. Teams work on them as on
// GitHub's, named git:<name> in their repos.

// gitPrefix marks a team's repo as one of these, not GitHub's.
const gitPrefix = "git:"

// GitRepos are the network's git repositories, as the console has them.
type GitRepos interface {
	// Repos is them, as last heard.
	Repos() []gitremote.Repo
	// Refresh asks the console again.
	Refresh(ctx context.Context) error
}

// UseGitRepos gives the runner the network's git repositories; nil for none.
func (r *Runner) UseGitRepos(g GitRepos) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gitRepos = g
}

func (r *Runner) gitRepoSource() GitRepos {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gitRepos
}

// gitRepo is the network's git repository name, asking the console again
// if it isn't known (the owner may have just added it); ok false if there's
// none, with what's said about it.
func (r *Runner) gitRepo(name string) (gitremote.Repo, string) {
	g := r.gitRepoSource()
	if g == nil {
		return gitremote.Repo{}, "There are no git repositories on this network: the owner adds them on the console's Connectors page."
	}
	if repo, ok := gitremote.Find(g.Repos(), name); ok {
		return repo, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.Refresh(ctx); err != nil {
		r.logger.Printf("git repositories: asking the console: %v", err)
	}
	repos := g.Repos()
	if repo, ok := gitremote.Find(repos, name); ok {
		return repo, ""
	}
	var names []string
	for _, x := range repos {
		names = append(names, gitPrefix+x.Name)
	}
	if len(names) == 0 {
		return gitremote.Repo{}, "There are no git repositories on this network: the owner adds them on the console's Connectors page."
	}
	return gitremote.Repo{}, fmt.Sprintf("%s isn't one of this network's git repositories: %s.", gitPrefix+name, strings.Join(names, ", "))
}
