// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/gitremote"
)

// Which repos a team works on: set by whoever may arrange the team, from
// the repos the network's GitHub installation has. A team inside another
// works on the outer team's repos unless it names its own.

func withRepos(repos ...string) option {
	return func(cfg *config.Runner, _ *[]config.Persona) { cfg.GitHub.Repos = repos }
}

func TestATeamsLeadChoosesItsRepos(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withRepos("web", "docs", "infra"))
	withSales(t, n) // dana leads Sales

	n.human.say("GuildServ", "TEAM SET Engineering REPOS web,infra")
	n.human.expect("Engineering's repos", guildServSaying("method", "repos web, infra"))
	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/teams/Sales", map[string]any{"repos": []string{"docs"}})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/teams/Engineering", map[string]any{"repos": []string{"docs"}})
	out := expectStatus(t, n, "method", http.StatusBadRequest, "PATCH", "/v1/teams/Sales", map[string]any{"repos": []string{"secret-plans"}})
	if errorOf(out) == "" {
		t.Error("a repo the installation doesn't have should be refused with a reason")
	}

	if repos := strs(teamIn(t, n, "Engineering")["repos"]); !slices.Equal(repos, []string{"web", "infra"}) {
		t.Errorf("Engineering's repos = %v", repos)
	}
	_, gh := n.api("dana").call("GET", "/v1/github", nil)
	if available := strs(gh["repos"]); !slices.Equal(available, []string{"web", "docs", "infra"}) {
		t.Errorf("the repos teams can choose from = %v", available)
	}
}

func TestATeamInsideAnotherWorksOnItsRepos(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withRepos("web", "docs"))
	owner := n.api("method")
	owner.call("PATCH", "/v1/teams/Engineering", map[string]any{"repos": []string{"web"}})
	owner.call("POST", "/v1/teams", map[string]any{"name": "Frontend", "parent": "Engineering"})
	owner.call("POST", "/v1/teams", map[string]any{"name": "Docs", "parent": "Engineering", "repos": []string{"docs"}})

	if got := n.runner.ReposFor("Frontend"); !slices.Equal(got, []string{"web"}) {
		t.Errorf("Frontend works on %v, want Engineering's web", got)
	}
	if got := n.runner.ReposFor("Docs"); !slices.Equal(got, []string{"docs"}) {
		t.Errorf("Docs works on %v, want its own docs", got)
	}
	if got := n.runner.ReposFor("Marketing"); len(got) != 0 {
		t.Errorf("Marketing works on %v, want none", got)
	}
}

// The web app links "PR #42" in a team's channel to the team's repo, so it's
// told the repos of each channel the org chart puts someone in (their full
// names, account and repo), and only those.
func TestYourChannelsReposThroughTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withRepos("web", "docs"), func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.GitHub.Owner = "acme-inc"
	})
	withSales(t, n) // dana leads Sales
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	owner := n.api("method")
	owner.call("PATCH", "/v1/teams/Engineering", map[string]any{"repos": []string{"web"}})
	owner.call("PATCH", "/v1/teams/Sales", map[string]any{"repos": []string{"docs"}})

	repos := func(actor string) map[string][]string {
		t.Helper()
		_, out := n.api(actor).call("GET", "/v1/me/channels", nil)
		got := map[string][]string{}
		for ch, rs := range out["repos"].(map[string]any) {
			got[ch] = strs(rs)
		}
		return got
	}
	if got := repos("sam"); len(got) != 1 || !slices.Equal(got["#engineering"], []string{"acme-inc/web"}) {
		t.Errorf("sam's channels' repos = %v, want #engineering's acme-inc/web only", got)
	}
	if got := repos("dana"); len(got) != 1 || !slices.Equal(got["#sales"], []string{"acme-inc/docs"}) {
		t.Errorf("dana's channels' repos = %v, want #sales's acme-inc/docs only", got)
	}
}

// A network whose guildnet.yaml said which repos each team works on
// (github.teams) keeps them: they're carried into the org once, and after
// that the org is what counts.
func TestTeamsKeepTheReposTheConfigFileGaveThem(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withRepos("web", "docs"), func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.GitHub.Teams = map[string][]string{"Engineering": {"web"}, "Marketing": {"docs"}}
	})

	if got := n.runner.ReposFor("Engineering"); !slices.Equal(got, []string{"web"}) {
		t.Errorf("Engineering works on %v, want web from the config file", got)
	}
	n.human.say("GuildServ", "TEAM SET Engineering REPOS docs")
	n.human.expect("changed in the org", guildServSaying("method", "repos docs"))
	n.restartRunner()
	if got := n.runner.ReposFor("Engineering"); !slices.Equal(got, []string{"docs"}) {
		t.Errorf("after a restart Engineering works on %v, want docs: the org counts, not the file", got)
	}
}

// gitRepos is the network's git repositories on any host, as the console
// has them: later, after a refresh, has more.
type gitRepos struct {
	mu        sync.Mutex
	now       []gitremote.Repo
	later     []gitremote.Repo
	refreshes int
}

func (g *gitRepos) Repos() []gitremote.Repo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.now)
}

func (g *gitRepos) Refresh(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refreshes++
	g.now = append(g.now, g.later...)
	g.later = nil
	return nil
}

// Teams work on git repositories on any host too, named git:<name>, with
// GitHub's or without.
func TestATeamWorksOnGitRepositoriesFromAnyHost(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withRepos("web", "docs"))
	git := &gitRepos{now: []gitremote.Repo{{Name: "infra", URL: "https://git.example.com/ops/infra.git", Read: true, Write: true}}}
	n.runner.UseGitRepos(git)

	n.human.say("GuildServ", "TEAM SET Engineering REPOS web,git:infra")
	n.human.expect("Engineering's repos", guildServSaying("method", "repos web, git:infra"))
	out := expectStatus(t, n, "method", http.StatusBadRequest, "PATCH", "/v1/teams/Marketing", map[string]any{"repos": []string{"git:nope"}})
	if !strings.Contains(errorOf(out), "infra") {
		t.Errorf("a git repository the network doesn't have: %q, want the ones it has", errorOf(out))
	}
	if got := n.runner.ReposFor("Engineering"); !slices.Equal(got, []string{"web", "git:infra"}) {
		t.Errorf("Engineering works on %v", got)
	}
}

func TestATeamWorksOnGitRepositoriesWithoutGitHub(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	n.runner.UseGitRepos(&gitRepos{now: []gitremote.Repo{{Name: "infra", URL: "https://git.example.com/ops/infra.git", Read: true}}})

	n.human.say("GuildServ", "TEAM SET Engineering REPOS git:infra")

	n.human.expect("Engineering's repos", guildServSaying("method", "repos git:infra"))
}

// A repository the owner has just added is known at once: a name the
// runner hasn't heard of has it ask again before refusing.
func TestAJustAddedGitRepositoryIsKnownAtOnce(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	git := &gitRepos{later: []gitremote.Repo{{Name: "infra", URL: "https://git.example.com/ops/infra.git", Read: true}}}
	n.runner.UseGitRepos(git)

	expectStatus(t, n, "method", http.StatusOK, "PATCH", "/v1/teams/Engineering", map[string]any{"repos": []string{"git:infra"}})

	git.mu.Lock()
	defer git.mu.Unlock()
	if git.refreshes != 1 {
		t.Errorf("asked again %d times, want once", git.refreshes)
	}
}

// A customer's network learns its repos from its GitHub installation while
// it runs: connecting GitHub, or giving it more repos, needs no restart.
func TestTheReposToChooseFromFollowTheInstallation(t *testing.T) {
	account, repos := "", []string(nil)
	n := startNetwork(t, &fakeBrain{}, withAPI(), func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.GitHubLive = func() (string, []string) { return account, repos }
	})
	if _, gh := n.api("method").call("GET", "/v1/github", nil); gh["connected"] != false {
		t.Errorf("before connecting: %v", gh)
	}

	account, repos = "acme-inc", []string{"api"}

	n.human.say("GuildServ", "TEAM SET Engineering REPOS api")
	n.human.expect("Engineering's repos", guildServSaying("method", "repos api"))
	if _, gh := n.api("method").call("GET", "/v1/github", nil); gh["account"] != "acme-inc" || gh["connected"] != true {
		t.Errorf("after connecting: %v", gh)
	}
}
