// SPDX-License-Identifier: AGPL-3.0-only

package gitwire_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/gittest"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
)

var (
	reader = gitwire.Credential{User: gittest.User, Token: gittest.ReadToken}
	writer = gitwire.Credential{User: gittest.User, Token: gittest.WriteToken}
)

func push(t *testing.T, s *gittest.Server, cred gitwire.Credential, u gitwire.Update, pack []byte) (gitwire.Result, error) {
	t.Helper()
	return gitwire.Push(context.Background(), s.Client(), s.URL(), cred, u, bytes.NewReader(pack))
}

// A new branch arrives with its commits, and nothing else moves.
func TestPushingMakesANewBranch(t *testing.T) {
	s := gittest.New(t)
	w := s.Clone()
	c := w.Commit(s.Main, "work.txt", "done\n")
	if _, err := push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/x", New: c}, w.Pack(c, s.Main)); err != nil {
		t.Fatal(err)
	}
	if got := s.Ref("refs/heads/kai/x"); got != c {
		t.Errorf("kai/x is %q, want %s", got, c)
	}
	if got := s.Ref("refs/heads/main"); got != s.Main {
		t.Errorf("main moved to %s", got)
	}
	if !s.Has(c + ":work.txt") {
		t.Error("the server doesn't have the commit's file")
	}
}

// A branch moves on from where it was.
func TestPushingMovesABranchOn(t *testing.T) {
	s := gittest.New(t)
	w := s.Clone()
	c1 := w.Commit(s.Main, "work.txt", "one\n")
	if _, err := push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/x", New: c1}, w.Pack(c1, s.Main)); err != nil {
		t.Fatal(err)
	}
	c2 := w.Commit(c1, "work.txt", "two\n")
	if _, err := push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/x", Old: c1, New: c2}, w.Pack(c2, c1)); err != nil {
		t.Fatal(err)
	}
	if got := s.Ref("refs/heads/kai/x"); got != c2 {
		t.Errorf("kai/x is %q, want %s", got, c2)
	}
}

// A branch that isn't where the pusher thought is left alone, whether it
// had moved before the push began or moves while it's under way.
func TestAPushThatLosesARaceChangesNothing(t *testing.T) {
	s := gittest.New(t)
	w := s.Clone()
	theirs := w.Commit(s.Main, "theirs.txt", "theirs\n")
	ours := w.Commit(s.Main, "ours.txt", "ours\n")

	w.Send("refs/heads/kai/x", theirs)
	_, err := push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/x", New: ours}, w.Pack(ours, s.Main))
	if !errors.Is(err, gitwire.ErrMoved) {
		t.Errorf("pushing over a branch someone made: %v, want ErrMoved", err)
	}
	if got := s.Ref("refs/heads/kai/x"); got != theirs {
		t.Errorf("kai/x is %s, want theirs, %s", got, theirs)
	}

	s.BeforePush(func() { w.Send("refs/heads/kai/y", theirs) })
	_, err = push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/y", New: ours}, w.Pack(ours, s.Main))
	var rejected *gitwire.RejectedError
	if !errors.As(err, &rejected) {
		t.Errorf("pushing as someone else makes the branch: %v, want a rejection", err)
	}
	if got := s.Ref("refs/heads/kai/y"); got != theirs {
		t.Errorf("kai/y is %s, want theirs, %s", got, theirs)
	}
}

// The host's hooks can refuse a push, and say why.
func TestTheHostCanRefuseAPush(t *testing.T) {
	s := gittest.New(t)
	s.Hook("pre-receive", `echo "branches here start with feature/" >&2; exit 1`)
	w := s.Clone()
	c := w.Commit(s.Main, "work.txt", "done\n")
	_, err := push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/x", New: c}, w.Pack(c, s.Main))
	var rejected *gitwire.RejectedError
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Said, "branches here start with feature/") {
		t.Fatalf("a refused push: %v (%+v), want the host's reason", err, rejected)
	}
	if got := s.Ref("refs/heads/kai/x"); got != "" {
		t.Errorf("kai/x was made, at %s", got)
	}
}

// What the host says (often where to open a merge request) comes back.
func TestWhatTheHostSaysComesBack(t *testing.T) {
	s := gittest.New(t)
	s.Hook("post-receive", `echo "To open a merge request, visit: https://git.example.com/repo/new"`)
	w := s.Clone()
	c := w.Commit(s.Main, "work.txt", "done\n")
	res, err := push(t, s, writer, gitwire.Update{Ref: "refs/heads/kai/x", New: c}, w.Pack(c, s.Main))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Said, "https://git.example.com/repo/new") {
		t.Errorf("the host said %q", res.Said)
	}
}

// Pushing takes a credential that may push.
func TestOnlyACredentialThatMayPushCanPush(t *testing.T) {
	s := gittest.New(t)
	w := s.Clone()
	c := w.Commit(s.Main, "work.txt", "done\n")
	for _, cred := range []gitwire.Credential{reader, {User: gittest.User, Token: "wrong"}, {}} {
		if _, err := push(t, s, cred, gitwire.Update{Ref: "refs/heads/kai/x", New: c}, w.Pack(c, s.Main)); !errors.Is(err, gitwire.ErrDenied) {
			t.Errorf("pushing with %q: %v, want ErrDenied", cred.Token, err)
		}
	}
	if got := s.Ref("refs/heads/kai/x"); got != "" {
		t.Errorf("kai/x was made, at %s", got)
	}
}

// Only branches are pushed: no tags, no other refs, and nothing deleted;
// nothing is sent for those.
func TestOnlyBranchesArePushed(t *testing.T) {
	s := gittest.New(t)
	w := s.Clone()
	c := w.Commit(s.Main, "work.txt", "done\n")
	zero := strings.Repeat("0", len(c))
	for _, u := range []gitwire.Update{
		{Ref: "refs/tags/v1", New: c},
		{Ref: "refs/notes/commits", New: c},
		{Ref: "kai/x", New: c},
		{Ref: "refs/heads/kai/../main", New: c},
		{Ref: "refs/heads/kai/x.lock", New: c},
		{Ref: "refs/heads/kai x", New: c},
		{Ref: "refs/heads/kai/x", Old: s.Main, New: zero},
		{Ref: "refs/heads/kai/x", Old: s.Main},
		{Ref: "refs/heads/kai/x", New: "not-a-commit"},
	} {
		if _, err := push(t, s, writer, u, w.Pack(c, s.Main)); err == nil {
			t.Errorf("%+v was pushed", u)
		}
	}
	if s.Requests != 0 {
		t.Errorf("the server had %d requests", s.Requests)
	}
}

// Credentials go only over HTTPS, and not in the address.
func TestOnlyHTTPSAddressesAreUsed(t *testing.T) {
	asked := 0
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked++ }))
	defer plain.Close()
	for _, repo := range []string{plain.URL + "/repo.git", "https://git:token@git.example.com/repo.git", "git.example.com/repo.git", "https:///repo.git"} { // trufflehog:ignore (made up)
		if _, err := gitwire.List(context.Background(), plain.Client(), repo, writer, gitwire.Fetching); err == nil {
			t.Errorf("%s was used", repo)
		}
	}
	if asked != 0 {
		t.Errorf("the plain server was asked %d times", asked)
	}
}

// Listing a repository's refs says where its branches are and which is its
// default; listing for pushing tells which credentials may push.
func TestListingSaysWhatTheRepositoryHas(t *testing.T) {
	s := gittest.New(t)
	ctx := context.Background()
	refs, err := gitwire.List(ctx, s.Client(), s.URL(), reader, gitwire.Fetching)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Refs["refs/heads/main"] != s.Main || refs.Head != "refs/heads/main" {
		t.Errorf("listed %v, HEAD %q; want main at %s and HEAD main", refs.Refs, refs.Head, s.Main)
	}
	if _, err := gitwire.List(ctx, s.Client(), s.URL(), reader, gitwire.Pushing); !errors.Is(err, gitwire.ErrDenied) {
		t.Errorf("listing for pushing with the read token: %v, want ErrDenied", err)
	}
	refs, err = gitwire.List(ctx, s.Client(), s.URL(), writer, gitwire.Pushing)
	if err != nil || refs.Refs["refs/heads/main"] != s.Main {
		t.Errorf("listing for pushing with the write token: %v, %v", refs, err)
	}
	if _, err := gitwire.List(ctx, s.Client(), strings.TrimSuffix(s.URL(), "repo.git")+"other.git", reader, gitwire.Fetching); !errors.Is(err, gitwire.ErrNotFound) {
		t.Errorf("listing a repository that isn't there: %v, want ErrNotFound", err)
	}
}

// A public repository is read with no credential at all.
func TestAPublicRepositoryIsReadWithoutACredential(t *testing.T) {
	s := gittest.New(t)
	s.AnyoneReads()
	refs, err := gitwire.List(context.Background(), s.Client(), s.URL(), gitwire.Credential{}, gitwire.Fetching)
	if err != nil || refs.Refs["refs/heads/main"] != s.Main {
		t.Errorf("listing a public repository: %v, %v", refs, err)
	}
}

// Somewhere that isn't a git server, or that sends us elsewhere, is said so,
// and the credential doesn't follow.
func TestAnAddressThatIsntAGitServerIsSaidSo(t *testing.T) {
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the credential followed a redirect to %s", r.URL)
	}))
	defer elsewhere.Close()
	web := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/moved") {
			http.Redirect(w, r, elsewhere.URL+"/repo.git/info/refs", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>sign in</html>"))
	}))
	defer web.Close()
	ctx := context.Background()
	if _, err := gitwire.List(ctx, web.Client(), web.URL+"/repo.git", reader, gitwire.Fetching); !errors.Is(err, gitwire.ErrNotGit) {
		t.Errorf("a web page: %v, want ErrNotGit", err)
	}
	_, err := gitwire.List(ctx, web.Client(), web.URL+"/moved/repo.git", reader, gitwire.Fetching)
	if err == nil || !strings.Contains(err.Error(), elsewhere.URL) {
		t.Errorf("a redirect: %v, want where it points", err)
	}
}
