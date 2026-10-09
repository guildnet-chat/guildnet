// SPDX-License-Identifier: AGPL-3.0-only

package gitremote_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/pem"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/gitremote"
	"github.com/guildnet-chat/guildnet/internal/gittest"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// End to end, when GUILDNET_DOCKER_TESTS=1: an agent's desk in real Docker
// clones one of its team's repositories from a git host that isn't on the
// list every desk has, through the desks' proxy, with its network's egress
// pass and the repository's read token; the agent commits there, and the
// runner pushes its branch with the push token. The host gets the branch,
// with the agent's commit, and main is as it was. A desk without the pass
// can't reach the host at all.

const gitE2EImage = "guildnet-desk-git-e2e:latest"

// fakeGit is the team's git repositories, as the desk checks them out.
type fakeGit []desk.Clone

func (g fakeGit) Clones(context.Context, config.Persona) ([]desk.Clone, error) { return g, nil }

type logged struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logged) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestAnAgentClonesCommitsAndPushesThroughItsDeskInRealDocker(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	s := gittest.New(t)
	// The host is example.com (the test server's certificate is for it),
	// which the proxy and the runner reach on this machine.
	host := "example.com:" + s.URL()[strings.LastIndex(s.URL(), ":")+1:strings.Index(s.URL(), "/repo.git")]
	repoURL := "https://" + host + "/repo.git"

	// A desk image that trusts the test server's certificate.
	cert := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	build := exec.Command("docker", "build", "--quiet", "--tag", gitE2EImage, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\nRUN apk add --no-cache bash coreutils git && adduser -u 1000 -D -h /home/desk desk && mkdir -p /workspace && chown desk:desk /workspace && echo " +
		cert + " | base64 -d > /etc/gittest.pem\nENV GIT_SSL_CAINFO=/etc/gittest.pem\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the image: %v: %s", err, out)
	}
	const network = "guildnet-git-e2e"
	exec.Command("docker", "network", "create", network).Run()
	// This machine, from a desk: the network's gateway on Linux; Docker
	// Desktop's name for it elsewhere.
	gateway := []byte("host.docker.internal")
	if runtime.GOOS == "linux" {
		var err error
		if gateway, err = exec.Command("docker", "network", "inspect", "-f", "{{(index .IPAM.Config 0).Gateway}}", network).Output(); err != nil {
			t.Fatalf("the desks' network: %v", err)
		}
	}

	// The desks' proxy, on this machine, where the desks' network's gateway
	// reaches it: it opens example.com only with acme's pass.
	public, private := sandbox.NewKeys()
	proxyLog := &logged{}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &http.Server{Handler: &sandbox.Proxy{Allowed: []string{"proxy.golang.org"}, Keys: []ed25519.PublicKey{public}, Logger: log.New(proxyLog, "", 0),
		Dial: egress.Dial(egress.Options{AllowLoopback: true, Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}})}}
	go proxy.Serve(ln)
	defer proxy.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	backend := &sandbox.Docker{Image: gitE2EImage, Network: network, Memory: "256m", CPUs: "1", Pids: 128,
		Proxy: "http://" + strings.TrimSpace(string(gateway)) + ":" + port}
	sandboxd := httptest.NewServer(sandbox.NewServer(sandbox.Options{Backend: backend, PublicKeys: []ed25519.PublicKey{public}}))
	defer sandboxd.Close()
	remote := desk.NewRemote(sandboxd.URL, sandbox.TokenFor(private, "acme", sandbox.Desks, time.Now(), 15*time.Minute))
	ws := desk.New(remote, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defer ws.Clear(context.Background(), kai)
	ws.Clear(ctx, kai)

	// Without the pass, the host can't be reached.
	ws.Git = fakeGit{{Name: "infra", URL: repoURL, Read: gitwire.Credential{User: gittest.User, Token: gittest.ReadToken}}}
	ws.Pass = func(context.Context) (string, error) { return "", nil }
	if err := ws.Ready(ctx, kai); err == nil {
		t.Fatal("a desk without its network's pass cloned from the host")
	}

	ws.Pass = func(context.Context) (string, error) {
		return sandbox.PassFor(private, "acme", []string{host}, time.Now(), 15*time.Minute), nil
	}
	if err := ws.Ready(ctx, kai); err != nil {
		t.Fatalf("readying the desk: %v\nthe proxy: %s", err, proxyLog)
	}
	out, err := ws.Run(ctx, kai, "bash", map[string]any{"command": "cd infra && git checkout -q -b kai/fix && echo fixed > fix.txt && git add fix.txt && git commit -q -m 'Fix it' && git rev-parse HEAD && cat ~/.config/guildnet/git/infra"})
	if err != nil || strings.Contains(out, gittest.ReadToken) {
		t.Fatalf("committing at the desk: %q, %v", out, err)
	}

	// The runner pushes, reaching example.com on this machine.
	transport := s.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.Replace(addr, "example.com", "127.0.0.1", 1))
	}
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/git":
			w.Write([]byte(`{"repos":[{"name":"infra","url":"` + repoURL + `","read":true,"write":true}]}`))
		case "/git/infra/write":
			w.Write([]byte(`{"url":"` + repoURL + `","user":"` + gittest.User + `","token":"` + gittest.WriteToken + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer broker.Close()
	live := &gitremote.Live{Broker: gitremote.Broker{URL: broker.URL, Network: "acme", Secret: "s"}}
	live.Refresh(ctx)
	p := &gitremote.Pusher{Live: live, Desk: remote, HTTP: &http.Client{Transport: transport}, TeamRepos: func(string) []string { return []string{"git:infra"} }}

	said, err := p.Run(ctx, kai, "git_push_branch", map[string]any{"repo": "infra", "branch": "kai/fix"})

	if err != nil {
		t.Fatalf("pushing: %v", err)
	}
	tip := s.Ref("refs/heads/kai/fix")
	if tip == "" || !strings.Contains(out, tip) || !strings.Contains(said, tip[:7]) {
		t.Errorf("the host has kai/fix at %q; the desk committed %q; the agent was told %q", tip, out, said)
	}
	if author := s.Show(tip, "%an <%ae>"); author != "Kai (kai, AI agent) <kai@agents.guildnet.dev>" {
		t.Errorf("the commit is %q's", author)
	}
	if got := s.Ref("refs/heads/main"); got != s.Main {
		t.Errorf("main moved to %s", got)
	}
	if !strings.Contains(proxyLog.String(), "tunnel to "+host) {
		t.Errorf("the desk didn't go through the proxy:\n%s", proxyLog)
	}
}
