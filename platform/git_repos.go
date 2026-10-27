// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Git repositories on any host (GitLab, Gitea, Bitbucket, a company's own
// server) that a network's agents work on at desks. The owner adds each on
// the console's Connectors page with two credentials: one to read, which
// reaches desks (so it should be a read-only deploy token), and one to
// push, which only the runner ever has, for a push. Both stay here,
// sealed; the network's runner gets them from the broker one at a time.

// GitRepo is one of a network's git repositories.
type GitRepo struct {
	Name string // what teams know it by
	URL  string // https://
	// Read and Write are the credentials to read and to push with; none
	// (the zero value) is a public repository's, or one agents can't push to.
	Read, Write gitwire.Credential
	By          string // who added it
	At          time.Time
}

// GitRepos keeps networks' git repositories.
type GitRepos interface {
	GitRepos(ctx context.Context, network string) ([]GitRepo, error)
	GitRepo(ctx context.Context, network, name string) (GitRepo, error) // ErrNotFound if none
	SaveGitRepo(ctx context.Context, network string, r GitRepo) error
	RemoveGitRepo(ctx context.Context, network, name string) error // ErrNotFound if none
}

var gitRepoName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// scpLike is git's other way of writing an SSH address: user@host:path.
var scpLike = regexp.MustCompile(`^[^/:@]+@[^/:]+:`)

// gitEgress is how the console reaches git hosts: public HTTPS addresses,
// and the private hosts whoever runs the install named.
func (p *Platform) gitEgress() egress.Options {
	o := p.opts.ConnectorEgress
	o.PrivateHosts = p.opts.GitPrivateHosts
	return o
}

// checkGitURL is raw as a repository's address, or what's wrong with it.
func (p *Platform) checkGitURL(raw string) (string, string) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	switch {
	case strings.HasPrefix(raw, "ssh://") || scpLike.MatchString(raw):
		return "", "SSH addresses aren't taken yet: use the repository's https:// address, with a token."
	case err == nil && u.Scheme == "http":
		return "", "Use the repository's https:// address: over http:// its credentials would be sent unencrypted."
	case err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "":
		return "", "That isn't a repository's https:// address."
	case u.User != nil:
		return "", "Leave the user name and token out of the address: they go in the boxes for them."
	case u.RawQuery != "" || u.Fragment != "":
		return "", "That isn't a repository's address: leave out anything after ? or #."
	}
	if err := p.gitEgress().Check(raw); err != nil {
		problem := u.Hostname() + " is a private address (or a name inside a network), which " + p.brand().Name + " doesn't reach."
		if p.opts.FirstRun != nil {
			problem += " Whoever runs it can allow a git server on your own network with GUILDNET_GIT_PRIVATE_HOSTS."
		}
		return "", problem
	}
	return raw, ""
}

// credential is a credential from a form's user and token: none without a
// token, and the user "git" if none's given (most hosts take any).
func credential(user, token string) gitwire.Credential {
	user, token = strings.TrimSpace(user), strings.TrimSpace(token)
	if token == "" {
		return gitwire.Credential{}
	}
	if user == "" {
		user = "git"
	}
	return gitwire.Credential{User: user, Token: token}
}

// gitCheck is how a repository's credentials did, tried on its host.
type gitCheck struct {
	Name           string
	Reads, Pushes  string // "yes", or why not
	Problem        string // what stops it being added, if anything
	Warning        string
	ReadOK, PushOK bool
}

// whyNot says what an error from the host comes to.
func whyNot(err error) string {
	switch {
	case errors.Is(err, gitwire.ErrDenied):
		return "the host refused the credential"
	case errors.Is(err, gitwire.ErrNotFound):
		return "there's no repository there, or the credential can't see it"
	case errors.Is(err, gitwire.ErrNotGit):
		return "the address doesn't answer as a git repository"
	}
	return oneLine(err.Error(), 200)
}

// checkGitRepo tries repo's credentials on its host: the read credential
// reads, and the write one may push. A read credential that may push too
// is a warning: it reaches desks.
func (p *Platform) checkGitRepo(ctx context.Context, repo GitRepo) gitCheck {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := gitCheck{Name: repo.Name}
	if _, err := gitwire.List(ctx, p.gitHTTP, repo.URL, repo.Read, gitwire.Fetching); err != nil {
		c.Reads = "no: " + whyNot(err)
		c.Problem = repo.Name + "'s read credential can't read it: " + whyNot(err) + "."
	} else {
		c.Reads, c.ReadOK = "yes", true
	}
	if repo.Write == (gitwire.Credential{}) {
		c.Pushes = "no: there's no credential to push with, so agents can't push to it"
	} else if _, err := gitwire.List(ctx, p.gitHTTP, repo.URL, repo.Write, gitwire.Pushing); err != nil {
		c.Pushes = "no: " + whyNot(err)
		if c.Problem == "" {
			c.Problem = repo.Name + "'s write credential can't push to it: " + whyNot(err) + "."
		}
	} else {
		c.Pushes, c.PushOK = "yes", true
	}
	if c.ReadOK && repo.Read != (gitwire.Credential{}) {
		if _, err := gitwire.List(ctx, p.gitHTTP, repo.URL, repo.Read, gitwire.Pushing); err == nil {
			c.Warning = repo.Name + "'s read credential can push too. Desks can reach it, so give it one that only reads (a read-only deploy token)."
		}
	}
	return c
}

// gitRepoView is a repository as the Connectors page shows it: never its
// credentials.
type gitRepoView struct {
	Name, URL     string
	Read, Write   bool
	At            time.Time
	Test, Warning string
}

func (p *Platform) gitRepoViews(ctx context.Context, network string) []gitRepoView {
	if p.opts.GitRepos == nil {
		return nil
	}
	repos, err := p.opts.GitRepos.GitRepos(ctx, network)
	if err != nil {
		monitor.Problem(p.opts.Logger, "%s's git repositories: %v", network, err)
	}
	var out []gitRepoView
	for _, r := range repos {
		out = append(out, gitRepoView{Name: r.Name, URL: r.URL, Read: r.Read != (gitwire.Credential{}), Write: r.Write != (gitwire.Credential{}), At: r.At})
	}
	return out
}

// gitForm is what the owner typed, shown again (but the tokens) when it
// wasn't taken.
type gitForm struct{ Name, URL, ReadUser, WriteUser string }

// addGitRepo adds a repository, or replaces one of the same name, once its
// credentials are tried on its host.
func (p *Platform) addGitRepo(w http.ResponseWriter, r *http.Request) {
	if p.opts.GitRepos == nil {
		http.NotFound(w, r)
		return
	}
	n := networkOf(r)
	form := gitForm{Name: strings.TrimSpace(r.PostFormValue("name")), URL: strings.TrimSpace(r.PostFormValue("url")),
		ReadUser: strings.TrimSpace(r.PostFormValue("read_user")), WriteUser: strings.TrimSpace(r.PostFormValue("write_user"))}
	again := func(status int, problem string) {
		p.showConnectorsWith(w, r, status, problem, func(v *connectorsPage) { v.GitForm = form })
	}
	if !gitRepoName.MatchString(form.Name) {
		again(http.StatusBadRequest, "Name it with up to 40 lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit.")
		return
	}
	addr, problem := p.checkGitURL(form.URL)
	if problem != "" {
		again(http.StatusBadRequest, problem)
		return
	}
	repo := GitRepo{Name: form.Name, URL: addr, Read: credential(form.ReadUser, r.PostFormValue("read_token")),
		Write: credential(form.WriteUser, r.PostFormValue("write_token")), By: byAccount(r), At: p.opts.Now()}
	check := p.checkGitRepo(r.Context(), repo)
	if check.Problem != "" {
		again(http.StatusBadRequest, check.Problem)
		return
	}
	_, err := p.opts.GitRepos.GitRepo(r.Context(), n.Name, repo.Name)
	what := "added"
	if err == nil {
		what = "changed"
	}
	if err := p.opts.GitRepos.SaveGitRepo(r.Context(), n.Name, repo); err != nil {
		monitor.Problem(p.opts.Logger, "adding %s's git repository %s: %v", n.Name, repo.Name, err)
		again(http.StatusInternalServerError, "Couldn't save the repository. Try again.")
		return
	}
	p.opts.Logger.Printf("%s's git repository %s was %s", n.Name, repo.Name, what)
	p.tellOwner(r.Context(), n, gitRepoEmail(n, p.networkOrigin(r, n.Name)+"/connectors#git", what, repo, inConsole(r), p.opts.Now()))
	if check.Warning != "" {
		p.showConnectorsWith(w, r, http.StatusOK, "", func(v *connectorsPage) { v.Tested = &check })
		return
	}
	http.Redirect(w, r, "/connectors#git", http.StatusSeeOther)
}

// testGitRepo tries a repository's credentials on its host again.
func (p *Platform) testGitRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := p.gitRepoOf(w, r)
	if !ok {
		return
	}
	check := p.checkGitRepo(r.Context(), repo)
	p.showConnectorsWith(w, r, http.StatusOK, "", func(v *connectorsPage) { v.Tested = &check })
}

// removeGitRepo forgets a repository and its credentials.
func (p *Platform) removeGitRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := p.gitRepoOf(w, r)
	if !ok {
		return
	}
	n := networkOf(r)
	if err := p.opts.GitRepos.RemoveGitRepo(r.Context(), n.Name, repo.Name); err != nil && !errors.Is(err, ErrNotFound) {
		monitor.Problem(p.opts.Logger, "removing %s's git repository %s: %v", n.Name, repo.Name, err)
		p.showConnectors(w, r, http.StatusInternalServerError, "Something went wrong. Try again in a moment.")
		return
	}
	p.opts.Logger.Printf("%s's git repository %s was removed", n.Name, repo.Name)
	p.tellOwner(r.Context(), n, gitRepoEmail(n, p.networkOrigin(r, n.Name)+"/connectors#git", "removed", repo, inConsole(r), p.opts.Now()))
	http.Redirect(w, r, "/connectors#git", http.StatusSeeOther)
}

// gitRepoOf is the repository the address names, or the page saying
// there's none.
func (p *Platform) gitRepoOf(w http.ResponseWriter, r *http.Request) (GitRepo, bool) {
	if p.opts.GitRepos == nil {
		http.NotFound(w, r)
		return GitRepo{}, false
	}
	repo, err := p.opts.GitRepos.GitRepo(r.Context(), networkOf(r).Name, r.PathValue("name"))
	if errors.Is(err, ErrNotFound) {
		http.Redirect(w, r, "/connectors#git", http.StatusSeeOther)
		return repo, false
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "%s's git repository %s: %v", networkOf(r).Name, r.PathValue("name"), err)
		p.showConnectors(w, r, http.StatusInternalServerError, "Couldn't read the repository's credentials. Add it again.")
		return repo, false
	}
	return repo, true
}

// gitRepoEmail alerts the owner that a repository was added, changed (its
// address or credentials) or removed.
func gitRepoEmail(n Network, page, what string, repo GitRepo, where string, at time.Time) Letter {
	to := map[string]string{"added": "to", "changed": "on", "removed": "from"}[what]
	said := repo.Name + " was " + what + " " + to + " " + n.Name
	return Letter{Subject: said,
		Heading: said,
		Paras: []string{fmt.Sprintf("The git repository %s (%s) was %s %s %s from %s, at %s.", repo.Name, repo.URL, what, to, n.Name, where, stamp(at)),
			notYou},
		Button: "Open Connectors", Link: page,
		Why: securityWhy,
	}
}

// The broker's part: a runner's repositories, and their credentials.

// brokerGitRepos lists the calling network's repositories, for its runner:
// never their credentials.
func (p *Platform) brokerGitRepos(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	list := []map[string]any{}
	for _, v := range p.gitRepoViews(r.Context(), n.Name) {
		list = append(list, map[string]any{"name": v.Name, "url": v.URL, "read": v.Read, "write": v.Write})
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"repos": list})
}

// brokerGitCredential gives the calling network's runner one of its
// repository's credentials, to read or to push with, for one call.
func (p *Platform) brokerGitCredential(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	access := r.PathValue("access")
	if p.opts.GitRepos == nil || !slices.Contains([]string{"read", "write"}, access) {
		brokerError(w, http.StatusNotFound, "no such repository")
		return
	}
	repo, err := p.opts.GitRepos.GitRepo(r.Context(), n.Name, r.PathValue("name"))
	if errors.Is(err, ErrNotFound) {
		brokerError(w, http.StatusNotFound, "no such repository")
		return
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "%s's git repository %s, for its runner: %v", n.Name, r.PathValue("name"), err)
		brokerError(w, http.StatusInternalServerError, "couldn't read the repository's credentials")
		return
	}
	cred := repo.Read
	if access == "write" {
		if cred = repo.Write; cred == (gitwire.Credential{}) {
			brokerError(w, http.StatusNotFound, repo.Name+" has no credential to push with: the network's owner adds one on the console's Connectors page")
			return
		}
	}
	p.opts.Logger.Printf("git credential for %s: %s, %s", n.Name, repo.Name, access)
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]any{"url": repo.URL, "user": cred.User, "token": cred.Token})
}

// Memory.

func (s *MemoryStore) GitRepos(_ context.Context, network string) ([]GitRepo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []GitRepo
	for k, r := range s.gitRepos {
		if strings.HasPrefix(k, strings.ToLower(network)+"|") {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b GitRepo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *MemoryStore) GitRepo(_ context.Context, network, name string) (GitRepo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.gitRepos[connKey(network, name)]
	if !ok {
		return GitRepo{}, ErrNotFound
	}
	return r, nil
}

func (s *MemoryStore) SaveGitRepo(_ context.Context, network string, r GitRepo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gitRepos == nil {
		s.gitRepos = map[string]GitRepo{}
	}
	s.gitRepos[connKey(network, r.Name)] = r
	return nil
}

func (s *MemoryStore) RemoveGitRepo(_ context.Context, network, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.gitRepos[connKey(network, name)]; !ok {
		return ErrNotFound
	}
	delete(s.gitRepos, connKey(network, name))
	return nil
}

// dropGitRepos forgets a removed network's repositories. Callers hold mu.
func (s *MemoryStore) dropGitRepos(network string) {
	for k := range s.gitRepos {
		if strings.HasPrefix(k, strings.ToLower(network)+"|") {
			delete(s.gitRepos, k)
		}
	}
}

// Postgres. The tokens are sealed, bound to the network, the repository,
// its address and the user they go with.

func gitTokenAAD(network, name, url, which, user string) []byte {
	return aad("git_repos", which+"_token", strings.ToLower(network), name, url, user)
}

const gitRepoColumns = `name, url, read_user, read_token, write_user, write_token, added_by, added_at`

func (s *Postgres) scanGitRepo(network string, row pgx.Row) (GitRepo, error) {
	var r GitRepo
	var read, write []byte
	if err := row.Scan(&r.Name, &r.URL, &r.Read.User, &read, &r.Write.User, &write, &r.By, &r.At); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	var err error
	if r.Read.Token, err = s.openString(read, gitTokenAAD(network, r.Name, r.URL, "read", r.Read.User)); err != nil {
		return r, fmt.Errorf("%s's git repository %s: %w", network, r.Name, err)
	}
	if r.Write.Token, err = s.openString(write, gitTokenAAD(network, r.Name, r.URL, "write", r.Write.User)); err != nil {
		return r, fmt.Errorf("%s's git repository %s: %w", network, r.Name, err)
	}
	return r, nil
}

func (s *Postgres) GitRepos(ctx context.Context, network string) ([]GitRepo, error) {
	rows, err := s.pool.Query(ctx, `select `+gitRepoColumns+` from git_repos where network = $1 order by name`, strings.ToLower(network))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitRepo
	for rows.Next() {
		r, err := s.scanGitRepo(network, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Postgres) GitRepo(ctx context.Context, network, name string) (GitRepo, error) {
	return s.scanGitRepo(network, s.pool.QueryRow(ctx, `select `+gitRepoColumns+` from git_repos where network = $1 and name = $2`,
		strings.ToLower(network), name))
}

func (s *Postgres) SaveGitRepo(ctx context.Context, network string, r GitRepo) error {
	read, err := s.sealString(r.Read.Token, gitTokenAAD(network, r.Name, r.URL, "read", r.Read.User))
	if err != nil {
		return err
	}
	write, err := s.sealString(r.Write.Token, gitTokenAAD(network, r.Name, r.URL, "write", r.Write.User))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into git_repos (network, `+gitRepoColumns+`) values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (network, name) do update set url = $3, read_user = $4, read_token = $5, write_user = $6, write_token = $7, added_by = $8, added_at = $9`,
		strings.ToLower(network), r.Name, r.URL, r.Read.User, read, r.Write.User, write, r.By, r.At)
	return err
}

func (s *Postgres) RemoveGitRepo(ctx context.Context, network, name string) error {
	tag, err := s.pool.Exec(ctx, `delete from git_repos where network = $1 and name = $2`, strings.ToLower(network), name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
