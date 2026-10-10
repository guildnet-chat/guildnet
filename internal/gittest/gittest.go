// SPDX-License-Identifier: AGPL-3.0-only

// Package gittest is a real git server for tests: git's own http-backend
// behind HTTPS, with a read token and a write token, serving one bare
// repository whose main branch has one commit. Work is a clone of it to
// make commits and packs in, as a desk would.
package gittest

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The tokens the server takes, by what they may do.
const (
	ReadToken  = "read-token-for-tests"
	WriteToken = "write-token-for-tests"
	User       = "git"
)

// Server is a git server over HTTPS.
type Server struct {
	t      *testing.T
	srv    *httptest.Server
	bare   string
	env    []string
	mu     sync.Mutex
	before func()
	public bool
	// Main is main's commit when the server starts.
	Main string
	// Requests counts the requests the server has had.
	Requests int
}

// New is a git server, closed when the test ends. It skips the test where
// there's no git.
func New(t *testing.T) *Server {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git here")
	}
	root := t.TempDir()
	s := &Server{t: t, bare: filepath.Join(root, "repo.git")}
	// Nobody's own git settings (signing, line endings) get in.
	global := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	s.git("", "init", "-q", "--bare", "-b", "main", s.bare)
	w := s.Clone()
	s.Main = w.Commit("", "README", "hello\n")
	w.git("push", "-q", "origin", s.Main+":refs/heads/main")

	backend := filepath.Join(strings.TrimSpace(s.git("", "--exec-path")), "git-http-backend")
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Requests++
		before, public := s.before, s.public
		s.mu.Unlock()
		pushing := strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		user, token, _ := r.BasicAuth()
		switch {
		case public && !pushing && r.Header.Get("Authorization") == "":
		case user == "" && token == "":
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "who are you?", http.StatusUnauthorized)
			return
		case token != WriteToken && (pushing || token != ReadToken):
			http.Error(w, "not with that token", http.StatusForbidden)
			return
		}
		if before != nil && r.Method == http.MethodPost && pushing {
			before()
		}
		env := []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}
		if pushing {
			// http-backend lets a push in only from someone signed in.
			env = append(env, "REMOTE_USER="+user)
		}
		for _, e := range s.env {
			if strings.HasPrefix(e, "GIT_CONFIG_") {
				env = append(env, e)
			}
		}
		// CGI takes no chunked bodies (git sends them; so do we).
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		(&cgi.Handler{Path: backend, Root: "/", Env: env, Stderr: io.Discard}).ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the repository's address.
func (s *Server) URL() string { return s.srv.URL + "/repo.git" }

// Client is an HTTP client that trusts the server.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// AnyoneReads lets anyone read the repository without a credential, as a
// public repository's host does; a credential sent is still checked.
func (s *Server) AnyoneReads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.public = true
}

// Certificate is the server's TLS certificate, to trust.
func (s *Server) Certificate() *x509.Certificate { return s.srv.Certificate() }

// BeforePush runs f as each push arrives, before git takes it.
func (s *Server) BeforePush(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = f
}

// Hook installs a hook (pre-receive, post-receive, …) running script, a
// shell script.
func (s *Server) Hook(name, script string) {
	s.t.Helper()
	path := filepath.Join(s.bare, "hooks", name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

// Ref is ref's commit on the server, or "" if there's no such ref.
func (s *Server) Ref(ref string) string {
	s.t.Helper()
	out, err := s.run("--git-dir="+s.bare, "rev-parse", "-q", "--verify", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Show is git log's one line about rev on the server, in format.
func (s *Server) Show(rev, format string) string {
	s.t.Helper()
	return strings.TrimSpace(s.git("", "--git-dir="+s.bare, "log", "-1", "--format="+format, rev))
}

// SetHead makes ref the repository's default branch.
func (s *Server) SetHead(ref string) {
	s.t.Helper()
	s.git("", "--git-dir="+s.bare, "symbolic-ref", "HEAD", ref)
}

// CloneTo clones the repository's directory to dir, with origin pointing at
// the server, as a desk's checkout is.
func (s *Server) CloneTo(dir string) *Work {
	s.t.Helper()
	w := &Work{s: s, dir: dir}
	s.git("", "clone", "-q", s.bare, dir)
	w.git("remote", "set-url", "origin", s.URL())
	return w
}

// Env is the environment git runs in here: nobody's own settings.
func (s *Server) Env() []string { return s.env }

// Git runs git in the working copy.
func (w *Work) Git(args ...string) string {
	w.s.t.Helper()
	return strings.TrimSpace(w.git(args...))
}

// Has says whether the server has the object.
func (s *Server) Has(object string) bool {
	_, err := s.run("--git-dir="+s.bare, "cat-file", "-e", object)
	return err == nil
}

// Clone is a working copy of the repository, cloned from its directory.
func (s *Server) Clone() *Work {
	s.t.Helper()
	w := &Work{s: s, dir: filepath.Join(s.t.TempDir(), "work")}
	s.git("", "clone", "-q", s.bare, w.dir)
	return w
}

func (s *Server) run(args ...string) (string, error) {
	return s.runIn("", nil, args...)
}

func (s *Server) runIn(dir string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, s.env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, errs.Bytes())
	}
	return out.String(), nil
}

func (s *Server) git(dir string, args ...string) string {
	s.t.Helper()
	out, err := s.runIn(dir, nil, args...)
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

// Work is a working copy, where commits are made.
type Work struct {
	s   *Server
	dir string
}

func (w *Work) git(args ...string) string {
	w.s.t.Helper()
	return w.s.git(w.dir, args...)
}

// Commit makes a commit on parent ("" for the first), writing content to
// file, and is the commit.
func (w *Work) Commit(parent, file, content string) string {
	w.s.t.Helper()
	if parent != "" {
		w.git("checkout", "-q", "--detach", parent)
	}
	if err := os.WriteFile(filepath.Join(w.dir, file), []byte(content), 0o644); err != nil {
		w.s.t.Fatal(err)
	}
	w.git("add", file)
	w.git("commit", "-q", "-m", "change "+file)
	return strings.TrimSpace(w.git("rev-parse", "HEAD"))
}

// Send puts commit at ref on the server, whatever was there, as someone
// else pushing would.
func (w *Work) Send(ref, commit string) {
	w.s.t.Helper()
	w.git("push", "-q", "--force", "origin", commit+":"+ref)
}

// Pack is a pack of what tip has that the commits in have don't.
func (w *Work) Pack(tip string, have ...string) []byte {
	w.s.t.Helper()
	revs := tip + "\n"
	for _, h := range have {
		revs += "^" + h + "\n"
	}
	out, err := w.s.runIn(w.dir, []byte(revs), "pack-objects", "-q", "--stdout", "--revs")
	if err != nil {
		w.s.t.Fatal(err)
	}
	return []byte(out)
}
