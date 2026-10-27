// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// Desks have no route out: the only way out is the proxy, which reaches the
// allowed hosts, by name, on public addresses, and nothing else.

// echo is a server on this machine that answers each line with it back.
func echo(t *testing.T) (port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port
}

// names resolves every name to an address of the test's choosing.
func names(to string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(to)}, nil
	}
}

func proxy(t *testing.T, allowed []string, port string, o egress.Options) string {
	t.Helper()
	var p int
	fmt.Sscan(port, &p)
	srv := httptest.NewServer(&sandbox.Proxy{Allowed: allowed, Ports: []int{p}, Dial: egress.Dial(o)})
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// connect asks the proxy for a tunnel to target and, if it's opened, says a
// line through it.
func connect(t *testing.T, proxyAddr, target string) (status int, heard string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	fmt.Fprint(c, "hello\n")
	line, _ := br.ReadString('\n')
	return resp.StatusCode, line
}

func TestTheProxyReachesAllowedHosts(t *testing.T) {
	port := echo(t)
	p := proxy(t, []string{"proxy.golang.org", "*.githubusercontent.com"}, port, egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})

	for _, host := range []string{"proxy.golang.org", "objects.githubusercontent.com"} {
		status, heard := connect(t, p, net.JoinHostPort(host, port))
		if status != 200 || heard != "hello\n" {
			t.Errorf("%s: %d, heard %q", host, status, heard)
		}
	}
}

// The allowed hosts as people write them in GUILDNET_DESK_ALLOWED: with a
// space after a comma, say.
func TestTheProxyTakesHostsWrittenWithSpaces(t *testing.T) {
	port := echo(t)
	p := proxy(t, []string{"github.com", " proxy.golang.org ", ""}, port, egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})

	if status, _ := connect(t, p, net.JoinHostPort("proxy.golang.org", port)); status != 200 {
		t.Errorf("a host written with spaces round it: %d", status)
	}
}

func TestTheProxyRefusesOtherHosts(t *testing.T) {
	port := echo(t)
	p := proxy(t, []string{"proxy.golang.org", "*.githubusercontent.com"}, port, egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})

	for _, host := range []string{"evil.example", "githubusercontent.com.evil.example", "proxy.golang.org.evil.example", "127.0.0.1"} {
		if status, _ := connect(t, p, net.JoinHostPort(host, port)); status != http.StatusForbidden {
			t.Errorf("%s: %d, want refused", host, status)
		}
	}
}

// connectWith is connect, handing the proxy pass.
func connectWith(t *testing.T, proxyAddr, target, pass string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte("guildnet:" + pass))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// A network's desks reach its git repositories' hosts, beyond the list
// every desk has, with the network's egress pass: signed by the platform,
// naming them, good for a while. Nothing else opens them.
func TestANetworksPassOpensItsGitHosts(t *testing.T) {
	port := echo(t)
	public, private := sandbox.NewKeys()
	_, other := sandbox.NewKeys()
	now := time.Now()
	srv := httptest.NewServer(&sandbox.Proxy{Allowed: []string{"proxy.golang.org"}, Ports: []int{443}, Keys: []ed25519.PublicKey{public},
		Dial: egress.Dial(egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})})
	t.Cleanup(srv.Close)
	p := strings.TrimPrefix(srv.URL, "http://")
	acme := net.JoinHostPort("git.acme.example", port)
	pass := sandbox.PassFor(private, "acme", []string{acme, "gitlab.com"}, now, 15*time.Minute)

	if status := connectWith(t, p, acme, pass); status != http.StatusOK {
		t.Errorf("acme's git host with acme's pass: %d", status)
	}
	for what, status := range map[string]int{
		"without a pass":                  connectWith(t, p, acme, ""),
		"to a host it doesn't name":       connectWith(t, p, net.JoinHostPort("git.globex.example", port), pass),
		"to another port":                 connectWith(t, p, net.JoinHostPort("gitlab.com", port), pass),
		"with a pass past its time":       connectWith(t, p, acme, sandbox.PassFor(private, "acme", []string{acme}, now.Add(-time.Hour), 15*time.Minute)),
		"with a pass someone else signed": connectWith(t, p, acme, sandbox.PassFor(other, "acme", []string{acme}, now, 15*time.Minute)),
		"with a desks token":              connectWith(t, p, acme, sandbox.TokenFor(private, "acme", sandbox.Desks, now, 15*time.Minute)),
	} {
		if status != http.StatusForbidden {
			t.Errorf("%s: %d, want refused", what, status)
		}
	}
}

// An allowed name pointed at an inside address (the metadata server, the
// VM's network) is still refused.
func TestTheProxyRefusesInsideAddressesEvenForAllowedHosts(t *testing.T) {
	port := echo(t)
	for _, inside := range []string{"127.0.0.1", "169.254.169.254", "10.20.0.2"} {
		p := proxy(t, []string{"proxy.golang.org"}, port, egress.Options{Resolve: names(inside)})

		if status, _ := connect(t, p, net.JoinHostPort("proxy.golang.org", port)); status == http.StatusOK {
			t.Errorf("reached %s", inside)
		}
	}
}

func TestTheProxyOnlyOpensAllowedPorts(t *testing.T) {
	port := echo(t)
	p := proxy(t, []string{"proxy.golang.org"}, port, egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})

	if status, _ := connect(t, p, "proxy.golang.org:22"); status != http.StatusForbidden {
		t.Errorf("port 22: %d, want refused", status)
	}
}

// open opens a tunnel through the proxy at proxyAddr to target, and leaves
// it open: the connection, and its reader past the proxy's answer.
func open(t *testing.T, proxyAddr, target string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp.StatusCode
}

// A tunnel nothing goes through for a while is closed, so the shared proxy
// doesn't hold on to every tunnel a desk ever left open.
func TestATunnelLeftIdleIsClosed(t *testing.T) {
	port := echo(t)
	var n int
	fmt.Sscan(port, &n)
	srv := httptest.NewServer(&sandbox.Proxy{Allowed: []string{"proxy.golang.org"}, Ports: []int{n}, Idle: 300 * time.Millisecond,
		Dial: egress.Dial(egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})})
	t.Cleanup(srv.Close)
	c, br, status := open(t, strings.TrimPrefix(srv.URL, "http://"), net.JoinHostPort("proxy.golang.org", port))
	if status != http.StatusOK {
		t.Fatalf("opening: %d", status)
	}
	fmt.Fprint(c, "hello\n")
	if line, _ := br.ReadString('\n'); line != "hello\n" {
		t.Fatalf("heard %q", line)
	}

	time.Sleep(time.Second)

	c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(c, "still there?\n")
	if line, err := br.ReadString('\n'); err == nil {
		t.Errorf("an idle tunnel stayed open: heard %q", line)
	}
}

// One desk can't take every tunnel the proxy has: past its share, it's
// told to wait.
func TestOneDeskCantHoldEveryTunnel(t *testing.T) {
	port := echo(t)
	var n int
	fmt.Sscan(port, &n)
	srv := httptest.NewServer(&sandbox.Proxy{Allowed: []string{"proxy.golang.org"}, Ports: []int{n}, MaxTunnels: 3,
		Dial: egress.Dial(egress.Options{Resolve: names("127.0.0.1"), AllowLoopback: true})})
	t.Cleanup(srv.Close)
	addr, target := strings.TrimPrefix(srv.URL, "http://"), net.JoinHostPort("proxy.golang.org", port)
	for range 3 {
		if _, _, status := open(t, addr, target); status != http.StatusOK {
			t.Fatalf("a tunnel within the share: %d", status)
		}
	}

	if _, _, status := open(t, addr, target); status != http.StatusTooManyRequests {
		t.Errorf("a tunnel past the share: %d, want 429", status)
	}
}

// The npm registry is read through the proxy and can't be published to:
// a token planted in something an agent read can't publish the company's
// code as a package. GET and HEAD go to the registry, without the desk's
// credentials; anything else is refused.
func TestTheNpmRegistryIsReadOnly(t *testing.T) {
	var got []string
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"left-pad"}`)
	}))
	t.Cleanup(registry.Close)
	srv := httptest.NewServer(&sandbox.Proxy{Reads: map[string]string{"/npm/": registry.URL + "/"}, ReadClient: registry.Client()})
	t.Cleanup(srv.Close)

	read, _ := http.NewRequest("GET", srv.URL+"/npm/left-pad?write=true", nil)
	read.Header.Set("Authorization", "Bearer planted-token")
	resp, err := http.DefaultClient.Do(read)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"name":"left-pad"}` || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("reading left-pad: %d %q", resp.StatusCode, body)
	}

	for _, method := range []string{"PUT", "POST", "DELETE", "PATCH"} {
		req, _ := http.NewRequest(method, srv.URL+"/npm/left-pad", strings.NewReader(`{"_attachments":{}}`))
		req.Header.Set("Authorization", "Bearer planted-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s (publishing): %d, want refused", method, resp.StatusCode)
		}
	}
	if len(got) != 1 || got[0] != "GET /left-pad?write=true auth=" {
		t.Errorf("the registry got %q, want one GET without the desk's credentials", got)
	}
}

// The registry isn't on the default list of hosts to tunnel to: only the
// read-only way reaches it.
func TestDesksDontTunnelToTheNpmRegistry(t *testing.T) {
	for _, h := range sandbox.DefaultAllowed {
		if h == "registry.npmjs.org" {
			t.Error("desks can tunnel to registry.npmjs.org, and publish there")
		}
	}
}

// Only tunnels (HTTPS): a plain request through the proxy is refused.
func TestTheProxyOnlyTunnels(t *testing.T) {
	p := proxy(t, []string{"proxy.golang.org"}, "443", egress.Options{})

	resp, err := http.Get("http://" + p + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("%d, want refused", resp.StatusCode)
	}
}
