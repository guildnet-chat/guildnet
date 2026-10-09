// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is a small, stateful stand-in for the parts of the GitHub API
// GuildNet uses: installation tokens, the Git Data API (refs, commits,
// trees), pull requests, comments, and checks. Repos hold files by path, so
// tests can look at what a branch actually contains.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	next     int
	tokens   map[string]tokenGrant // token -> what it was minted with
	grants   []tokenGrant          // every token minted, in order
	repos    map[string]*fakeRepo  // "owner/repo"
	merges   []string              // "repo#n method"
	comment  []string              // "repo#n: body"
	searches []string              // code searches' q, in order

	noChecks   bool // the app lacks permission to read checks
	noStatuses bool // the installation wasn't granted commit statuses
}

type tokenGrant struct {
	Token        string
	Permissions  map[string]string
	Repositories []string
}

type fakeRepo struct {
	refs    map[string]string            // branch -> commit sha
	commits map[string]fakeCommit        // sha -> commit
	trees   map[string]map[string]string // sha -> path -> content
	pulls   []*fakePull
	checks  map[string][]fakeCheck // commit sha -> check runs
}

type fakeCommit struct {
	Tree, Message, Author, Email string
	Parents                      []string
}

type fakePull struct {
	Number              int
	Title, Body         string
	Head, Base          string
	State               string // open, closed
	Merged              bool
	Additions, Deletion int
}

type fakeCheck struct{ Name, Status, Conclusion string }

const owner = "acme-inc"

func newFakeGitHub(t *testing.T, repos ...string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, tokens: map[string]tokenGrant{}, repos: map[string]*fakeRepo{}}
	for _, name := range repos {
		r := &fakeRepo{refs: map[string]string{}, commits: map[string]fakeCommit{},
			trees: map[string]map[string]string{}, checks: map[string][]fakeCheck{}}
		r.trees["tree0"] = map[string]string{"README.md": "# " + name + "\n", "main.go": "package main\n"}
		r.commits["c0"] = fakeCommit{Tree: "tree0", Message: "initial"}
		r.refs["main"] = "c0"
		f.repos[owner+"/"+name] = r
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.mintToken)
	mux.HandleFunc("GET /app/installations/{id}", f.getInstallation)
	mux.HandleFunc("GET /repos/{owner}/{repo}", f.withRepo(f.getRepo, false))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/{ref...}", f.withRepo(f.getRef, false))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/commits/{sha}", f.withRepo(f.getCommit, false))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/trees", f.withRepo(f.createTree, true))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/commits", f.withRepo(f.createCommit, true))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/refs", f.withRepo(f.createRef, true))
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/git/refs/{ref...}", f.withRepo(f.updateRef, true))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", f.withRepo(f.listPulls, false))
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", f.withRepo(f.createPull, true))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{n}", f.withRepo(f.getPull, false))
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{n}", f.withRepo(f.editPull, true))
	mux.HandleFunc("PUT /repos/{owner}/{repo}/pulls/{n}/merge", f.withRepo(f.mergePull, true))
	mux.HandleFunc("PUT /repos/{owner}/{repo}/pulls/{n}/update-branch", f.withRepo(f.updateBranch, true))
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{n}/comments", f.withRepo(f.createComment, true))
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}/check-runs", f.withRepo(f.checkRuns, false))
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}/status", f.withRepo(f.status, false))
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", f.withRepo(f.getContents, false))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref...}", f.withRepo(f.getTree, false))
	mux.HandleFunc("GET /search/code", f.searchCode)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the fake's API base URL.
func (f *fakeGitHub) URL() string { return f.srv.URL + "/" }

func (f *fakeGitHub) id() string {
	f.next++
	return strconv.Itoa(f.next)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, 404, map[string]string{"message": "Not Found"})
}

func (f *fakeGitHub) mintToken(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeJSON(w, 401, map[string]string{"message": "an app JWT is required"})
		return
	}
	var body struct {
		Permissions  map[string]string `json:"permissions"`
		Repositories []string          `json:"repositories"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noStatuses && body.Permissions["statuses"] != "" {
		writeJSON(w, 422, map[string]string{"message": "The permissions requested are not granted to this installation."})
		return
	}
	g := tokenGrant{Token: "ghs_" + f.id(), Permissions: body.Permissions, Repositories: body.Repositories}
	f.tokens[g.Token] = g
	f.grants = append(f.grants, g)
	writeJSON(w, 201, map[string]any{"token": g.Token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
}

// withRepo resolves the repo, checks the caller's token, and holds the lock.
// A write needs a token that isn't limited to reading.
func (f *fakeGitHub) withRepo(h func(http.ResponseWriter, *http.Request, *fakeRepo), write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		token := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "token "), "Bearer ")
		g, ok := f.tokens[token]
		if !ok {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		if write && g.Permissions != nil && g.Permissions["contents"] == "read" {
			writeJSON(w, 403, map[string]string{"message": "Resource not accessible by integration"})
			return
		}
		repo, ok := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !ok {
			notFound(w)
			return
		}
		h(w, r, repo)
	}
}

func (f *fakeGitHub) getRepo(w http.ResponseWriter, r *http.Request, _ *fakeRepo) {
	writeJSON(w, 200, map[string]any{"name": r.PathValue("repo"), "full_name": r.PathValue("owner") + "/" + r.PathValue("repo"), "default_branch": "main"})
}

func refJSON(branch, sha string) map[string]any {
	return map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}}
}

func (f *fakeGitHub) getRef(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	branch := strings.TrimPrefix(r.PathValue("ref"), "heads/")
	sha, ok := repo.refs[branch]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, 200, refJSON(branch, sha))
}

func (f *fakeGitHub) getCommit(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	c, ok := repo.commits[r.PathValue("sha")]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, 200, map[string]any{"sha": r.PathValue("sha"), "tree": map[string]any{"sha": c.Tree}, "message": c.Message})
}

func (f *fakeGitHub) createTree(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	var body struct {
		BaseTree string `json:"base_tree"`
		Tree     []map[string]any
	}
	json.NewDecoder(r.Body).Decode(&body)
	files := map[string]string{}
	for p, c := range repo.trees[body.BaseTree] {
		files[p] = c
	}
	for _, e := range body.Tree {
		p, _ := e["path"].(string)
		sha, hasSHA := e["sha"]
		switch {
		case e["content"] != nil:
			files[p] = e["content"].(string)
		case hasSHA && sha == nil:
			delete(files, p)
		default:
			writeJSON(w, 422, map[string]string{"message": "tree entry needs content or sha: null"})
			return
		}
	}
	sha := "tree" + f.id()
	repo.trees[sha] = files
	writeJSON(w, 201, map[string]any{"sha": sha})
}

func (f *fakeGitHub) createCommit(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	var body struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
		Author  struct{ Name, Email string }
	}
	json.NewDecoder(r.Body).Decode(&body)
	if _, ok := repo.trees[body.Tree]; !ok {
		writeJSON(w, 422, map[string]string{"message": "no such tree"})
		return
	}
	sha := "c" + f.id()
	repo.commits[sha] = fakeCommit{Tree: body.Tree, Message: body.Message, Parents: body.Parents, Author: body.Author.Name, Email: body.Author.Email}
	writeJSON(w, 201, map[string]any{"sha": sha, "tree": map[string]any{"sha": body.Tree}})
}

func (f *fakeGitHub) createRef(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	var body struct{ Ref, SHA string }
	json.NewDecoder(r.Body).Decode(&body)
	branch := strings.TrimPrefix(body.Ref, "refs/heads/")
	if _, exists := repo.refs[branch]; exists {
		writeJSON(w, 422, map[string]string{"message": "Reference already exists"})
		return
	}
	repo.refs[branch] = body.SHA
	writeJSON(w, 201, refJSON(branch, body.SHA))
}

func (f *fakeGitHub) updateRef(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	var body struct {
		SHA   string
		Force bool
	}
	json.NewDecoder(r.Body).Decode(&body)
	branch := strings.TrimPrefix(r.PathValue("ref"), "heads/")
	old, ok := repo.refs[branch]
	if !ok {
		notFound(w)
		return
	}
	if !body.Force && !repo.descends(body.SHA, old) {
		writeJSON(w, 422, map[string]string{"message": "Update is not a fast forward"})
		return
	}
	repo.refs[branch] = body.SHA
	writeJSON(w, 200, refJSON(branch, body.SHA))
}

// descends says whether ancestor is sha or one of its ancestors.
func (r *fakeRepo) descends(sha, ancestor string) bool {
	todo := []string{sha}
	for len(todo) > 0 {
		s := todo[0]
		todo = todo[1:]
		if s == ancestor {
			return true
		}
		todo = append(todo, r.commits[s].Parents...)
	}
	return false
}

func (f *fakeGitHub) pullJSON(repo *fakeRepo, p *fakePull) map[string]any {
	return map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": p.State, "merged": p.Merged,
		"html_url":  fmt.Sprintf("https://github.com/%s/pulls/%d", owner, p.Number),
		"head":      map[string]any{"ref": p.Head, "sha": repo.refs[p.Head]},
		"base":      map[string]any{"ref": p.Base},
		"additions": p.Additions, "deletions": p.Deletion, "changed_files": 1, "mergeable": true,
	}
}

func (f *fakeGitHub) listPulls(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	head := r.URL.Query().Get("head") // owner:branch
	_, branch, _ := strings.Cut(head, ":")
	out := []map[string]any{}
	for _, p := range repo.pulls {
		if p.State == "open" && (branch == "" || p.Head == branch) {
			out = append(out, f.pullJSON(repo, p))
		}
	}
	writeJSON(w, 200, out)
}

func (f *fakeGitHub) createPull(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	var body struct{ Title, Body, Head, Base string }
	json.NewDecoder(r.Body).Decode(&body)
	if _, ok := repo.refs[body.Head]; !ok {
		writeJSON(w, 422, map[string]string{"message": "head branch does not exist"})
		return
	}
	p := &fakePull{Number: len(repo.pulls) + 1, Title: body.Title, Body: body.Body, Head: body.Head, Base: body.Base, State: "open"}
	repo.pulls = append(repo.pulls, p)
	writeJSON(w, 201, f.pullJSON(repo, p))
}

func (repo *fakeRepo) pull(r *http.Request) *fakePull {
	n, _ := strconv.Atoi(r.PathValue("n"))
	if n < 1 || n > len(repo.pulls) {
		return nil
	}
	return repo.pulls[n-1]
}

func (f *fakeGitHub) getPull(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	p := repo.pull(r)
	if p == nil {
		notFound(w)
		return
	}
	writeJSON(w, 200, f.pullJSON(repo, p))
}

func (f *fakeGitHub) mergePull(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	p := repo.pull(r)
	if p == nil {
		notFound(w)
		return
	}
	var body struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if p.State != "open" {
		writeJSON(w, 405, map[string]string{"message": "Pull Request is not mergeable"})
		return
	}
	if body.SHA != "" && body.SHA != repo.refs[p.Head] {
		writeJSON(w, 409, map[string]string{"message": "Head branch was modified. Review and try the merge again."})
		return
	}
	// Squash: main gets the branch's tree as one new commit.
	sha := "c" + f.id()
	repo.commits[sha] = fakeCommit{Tree: repo.commits[repo.refs[p.Head]].Tree, Message: p.Title, Parents: []string{repo.refs[p.Base]}}
	repo.refs[p.Base] = sha
	p.State, p.Merged = "closed", true
	f.merges = append(f.merges, fmt.Sprintf("%s#%d %s", r.PathValue("repo"), p.Number, body.MergeMethod))
	writeJSON(w, 200, map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

func (f *fakeGitHub) createComment(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	var body struct{ Body string }
	json.NewDecoder(r.Body).Decode(&body)
	f.comment = append(f.comment, fmt.Sprintf("%s#%s: %s", r.PathValue("repo"), r.PathValue("n"), body.Body))
	writeJSON(w, 201, map[string]any{"id": 1, "html_url": "https://github.com/x#comment"})
}

func (f *fakeGitHub) checkRuns(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	if f.noChecks {
		writeJSON(w, 403, map[string]string{"message": "Resource not accessible by integration"})
		return
	}
	sha := r.PathValue("ref")
	if b, ok := repo.refs[sha]; ok {
		sha = b
	}
	var runs []map[string]any
	for _, c := range repo.checks[sha] {
		runs = append(runs, map[string]any{"name": c.Name, "status": c.Status, "conclusion": c.Conclusion})
	}
	writeJSON(w, 200, map[string]any{"total_count": len(runs), "check_runs": runs})
}

func (f *fakeGitHub) status(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	writeJSON(w, 200, map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
}

// Helpers for tests to look at and change the fake's state.

func (f *fakeGitHub) file(repo, branch, path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	c, ok := r.commits[r.refs[branch]]
	if !ok {
		return "", false
	}
	content, ok := r.trees[c.Tree][path]
	return content, ok
}

func (f *fakeGitHub) head(repo, branch string) fakeCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	return r.commits[r.refs[branch]]
}

func (f *fakeGitHub) pulls(repo string) []fakePull {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePull
	for _, p := range f.repos[owner+"/"+repo].pulls {
		out = append(out, *p)
	}
	return out
}

func (f *fakeGitHub) setChecks(repo, branch string, checks ...fakeCheck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	r.checks[r.refs[branch]] = checks
}

// pushAsSomeoneElse moves a branch on by one commit, as a person would.
func (f *fakeGitHub) pushAsSomeoneElse(repo, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	sha := "c" + f.id()
	r.commits[sha] = fakeCommit{Tree: r.commits[r.refs[branch]].Tree, Message: "later push", Parents: []string{r.refs[branch]}}
	r.refs[branch] = sha
}

func (f *fakeGitHub) lastGrant() tokenGrant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants[len(f.grants)-1]
}

// firstGrant is the first token minted.
func (f *fakeGitHub) firstGrant() tokenGrant {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.grants) == 0 {
		return tokenGrant{}
	}
	return f.grants[0]
}

func (f *fakeGitHub) grantCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.grants)
}

// testKey is a throwaway RSA key in PEM, standing in for the app's key.
func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// denyChecks makes checks unreadable, as for an app without Checks: read.
func (f *fakeGitHub) denyChecks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noChecks = true
}

// getContents serves a file at a ref (a commit sha or a branch).
func (f *fakeGitHub) getContents(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "main" // the default branch
	}
	if sha, ok := repo.refs[ref]; ok {
		ref = sha
	}
	c, ok := repo.commits[ref]
	if !ok {
		notFound(w)
		return
	}
	content, ok := repo.trees[c.Tree][r.PathValue("path")]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, 200, map[string]any{"type": "file", "encoding": "base64", "path": r.PathValue("path"),
		"content": base64.StdEncoding.EncodeToString([]byte(content))})
}

// updateBranch merges a PR's base into its head, as GitHub's "Update
// branch" button does: refused if both sides changed the same file.
func (f *fakeGitHub) updateBranch(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	p := repo.pull(r)
	if p == nil {
		notFound(w)
		return
	}
	head, base := repo.refs[p.Head], repo.refs[p.Base]
	if repo.descends(head, base) {
		writeJSON(w, 202, map[string]string{"message": "Updating pull request branch."})
		return
	}
	// The merge base: the first commit on head's first-parent line that base also has.
	mb := head
	for mb != "" && !repo.descends(base, mb) {
		parents := repo.commits[mb].Parents
		if len(parents) == 0 {
			mb = ""
			break
		}
		mb = parents[0]
	}
	was, ours, theirs := repo.trees[repo.commits[mb].Tree], repo.trees[repo.commits[head].Tree], repo.trees[repo.commits[base].Tree]
	merged := map[string]string{}
	for path, content := range theirs {
		merged[path] = content
	}
	for _, path := range unionKeys(was, ours) {
		o, inOurs := ours[path]
		old, inWas := was[path]
		if inOurs == inWas && o == old {
			continue // we didn't change it
		}
		t, inTheirs := theirs[path]
		if (inTheirs != inWas || t != old) && (inTheirs != inOurs || t != o) {
			writeJSON(w, 422, map[string]string{"message": "merge conflict between base and head"})
			return
		}
		if inOurs {
			merged[path] = o
		} else {
			delete(merged, path)
		}
	}
	tree := "tree" + f.id()
	repo.trees[tree] = merged
	sha := "c" + f.id()
	repo.commits[sha] = fakeCommit{Tree: tree, Message: "Merge " + p.Base + " into " + p.Head, Parents: []string{head, base}}
	repo.refs[p.Head] = sha
	writeJSON(w, 202, map[string]string{"message": "Updating pull request branch."})
}

func unionKeys(a, b map[string]string) []string {
	var out []string
	for k := range a {
		out = append(out, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// getTree lists a branch's (or commit's) files, as a recursive tree.
func (f *fakeGitHub) getTree(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	ref := r.PathValue("ref")
	if sha, ok := repo.refs[ref]; ok {
		ref = sha
	}
	c, ok := repo.commits[ref]
	if !ok {
		notFound(w)
		return
	}
	var entries []map[string]any
	for _, p := range sortedKeys(repo.trees[c.Tree]) {
		entries = append(entries, map[string]any{"path": p, "type": "blob", "mode": "100644", "sha": "b" + p})
	}
	writeJSON(w, 200, map[string]any{"sha": c.Tree, "tree": entries, "truncated": false})
}

// searchCode finds files on main holding the query's words, in the repo
// its "repo:owner/name" names.
func (f *fakeGitHub) searchCode(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query().Get("q")
	f.searches = append(f.searches, q)
	var repoName string
	var words []string
	for _, w := range strings.Fields(q) {
		if name, ok := strings.CutPrefix(w, "repo:"); ok {
			repoName = name
			continue
		}
		words = append(words, w)
	}
	repo := f.repos[repoName]
	var items []map[string]any
	if repo != nil {
		files := repo.trees[repo.commits[repo.refs["main"]].Tree]
		for _, p := range sortedKeys(files) {
			for _, line := range strings.Split(files[p], "\n") {
				if len(words) > 0 && strings.Contains(line, words[0]) {
					_, name, _ := strings.Cut(repoName, "/")
					items = append(items, map[string]any{"name": p, "path": p, "repository": map[string]any{"name": name, "full_name": repoName},
						"text_matches": []map[string]any{{"fragment": line}}})
					break
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"total_count": len(items), "incomplete_results": false, "items": items})
}

func (f *fakeGitHub) lastSearch() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.searches) == 0 {
		return ""
	}
	return f.searches[len(f.searches)-1]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pushToMain commits a file straight to a repo's main, as a person merging
// something else would.
func (f *fakeGitHub) pushToMain(repo, path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	files := map[string]string{}
	for p, c := range r.trees[r.commits[r.refs["main"]].Tree] {
		files[p] = c
	}
	files[path] = content
	tree := "tree" + f.id()
	r.trees[tree] = files
	sha := "c" + f.id()
	r.commits[sha] = fakeCommit{Tree: tree, Message: "elsewhere", Parents: []string{r.refs["main"]}}
	r.refs["main"] = sha
}

func (f *fakeGitHub) editPull(w http.ResponseWriter, r *http.Request, repo *fakeRepo) {
	p := repo.pull(r)
	if p == nil {
		notFound(w)
		return
	}
	var body struct{ State string }
	json.NewDecoder(r.Body).Decode(&body)
	if body.State != "" {
		p.State = body.State
	}
	writeJSON(w, 200, f.pullJSON(repo, p))
}

// renameHead moves a PR onto another branch name, as for PRs opened before
// branch names were enforced.
func (f *fakeGitHub) renameHead(repo string, n int, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	p := r.pulls[n-1]
	r.refs[branch] = r.refs[p.Head]
	p.Head = branch
}

// getInstallation is what the installation was granted.
func (f *fakeGitHub) getInstallation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	perms := map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "checks": "read", "statuses": "read", "metadata": "read"}
	if f.noStatuses {
		delete(perms, "statuses")
	}
	writeJSON(w, 200, map[string]any{"id": 2, "account": map[string]any{"login": owner}, "permissions": perms})
}
