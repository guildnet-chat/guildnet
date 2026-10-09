// SPDX-License-Identifier: AGPL-3.0-only

package egress_test

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/egress"
)

// GuildNet calls addresses its customers choose (an agent's webhook). They
// must never reach inside: the cloud's metadata server, the VM, private
// networks. So only HTTPS, only public addresses, checked after resolving
// and dialed as checked (no DNS rebinding), and no redirects.

func TestOnlyPublicHTTPSAddressesAreTaken(t *testing.T) {
	for _, bad := range []string{
		"http://example.com/hook", "ftp://example.com/", "https://user:pw@example.com/", "https:///hook", "not a url",
		"https://10.0.0.5/hook", "https://169.254.169.254/computeMetadata/v1/", "https://127.0.0.1:8443/", "https://[::1]/",
		"https://[fd00::1]/", "https://100.64.0.1/", "https://0.0.0.0/", "https://localhost/", "https://metadata.google.internal/",
		// IPv6 ways of writing an IPv4 address inside: 6to4 and IPv4-compatible.
		"https://[2002:a9fe:a9fe::1]/", "https://[::a9fe:a9fe]/", "https://[::10.0.0.5]/",
		// A local NAT64 (RFC 8215), and the old site-local range.
		"https://[64:ff9b:1::a00:5]/", "https://[fec0::1]/",
	} {
		if err := egress.CheckURL(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	for _, good := range []string{"https://example.com/hook", "https://agents.example.com:8443/guildnet?x=1", "https://93.184.215.14/hook"} {
		if err := egress.CheckURL(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

// resolving answers name lookups from a fixed table.
func resolving(table map[string]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range strings.Split(table[host], ",") {
			if ip, err := netip.ParseAddr(a); err == nil {
				out = append(out, ip)
			}
		}
		return out, nil
	}
}

func TestANameThatResolvesInsideIsRefused(t *testing.T) {
	c := egress.Client(egress.Options{Resolve: resolving(map[string]string{
		"metadata.evil.test": "169.254.169.254",
		"vm.evil.test":       "10.20.0.2",
		"mixed.evil.test":    "93.184.215.14,127.0.0.1", // one bad address is enough to refuse
	})})
	for _, host := range []string{"metadata.evil.test", "vm.evil.test", "mixed.evil.test"} {
		_, err := c.Get("https://" + host + "/")
		if err == nil || !strings.Contains(err.Error(), "not a public address") {
			t.Errorf("%s: %v, want it refused as not public", host, err)
		}
	}
}

// server is an HTTPS server on this machine, which the tests allow as if it
// were public, and a client that trusts it.
func server(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	host := strings.TrimPrefix(srv.URL, "https://")
	ip, _ := netip.ParseAddrPort(host)
	c := egress.Client(egress.Options{RootCAs: pool, AllowLoopback: true,
		Resolve: resolving(map[string]string{"agent.example.com": ip.Addr().String()})})
	return srv, c
}

func TestAPublicAddressIsReachedByName(t *testing.T) {
	srv, c := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }))
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")

	resp, err := c.Get("https://agent.example.com:" + port + "/hook")

	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("reaching agent.example.com: %v", err)
	}
	resp.Body.Close()
}

func TestRedirectsArentFollowed(t *testing.T) {
	var elsewhere atomic.Bool
	srv, c := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			elsewhere.Store(true)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")

	resp, err := c.Get("https://agent.example.com:" + port + "/hook")

	if err == nil {
		resp.Body.Close()
	}
	if elsewhere.Load() {
		t.Error("the redirect was followed")
	}
}

// Hosts named as private (a self-hosted git server on the company's own
// network) are reached wherever they are; nothing else inside is.
func TestNamedPrivateHostsAreReachable(t *testing.T) {
	o := egress.Options{PrivateHosts: []string{"git.corp.local", "10.0.0.5"}}
	for _, good := range []string{"https://git.corp.local/team/repo.git", "https://GIT.corp.local:8443/repo.git", "https://10.0.0.5/repo.git"} {
		if err := o.Check(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"https://other.corp.local/repo.git", "https://10.0.0.6/", "http://git.corp.local/repo.git", "https://localhost/"} {
		if err := o.Check(bad); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")
	c := egress.Client(egress.Options{RootCAs: pool, PrivateHosts: []string{"example.com"},
		Resolve: resolving(map[string]string{"example.com": "127.0.0.1", "other.example.com": "127.0.0.1"})})
	resp, err := c.Get("https://example.com:" + port + "/")
	if err != nil {
		t.Fatalf("reaching the named host: %v", err)
	}
	resp.Body.Close()
	if _, err := c.Get("https://other.example.com:" + port + "/"); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("another host inside: %v, want it refused", err)
	}
}

// Loopback is only ever allowed when a test asks; by default even this
// machine's own HTTPS server is refused.
func TestThisMachineIsRefusedByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if _, err := egress.Client(egress.Options{}).Get(srv.URL); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("%s: %v", srv.URL, err)
	}
}
