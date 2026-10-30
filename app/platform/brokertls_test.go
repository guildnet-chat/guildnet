// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/pki"
)

// GUILDNET_SANDBOX_PIN is a pin, for sandboxd over TLS, or it's refused:
// one mistyped would otherwise pin nothing, or keep desks from working at
// all without saying why.
func TestASandboxPinMustBeOne(t *testing.T) {
	const pin = "sha256/47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	for _, c := range []struct {
		url, pin string
		ok       bool
	}{
		{"https://10.20.0.9:8443", pin, true},
		{"https://10.20.0.9:8443", "", true},
		{"https://10.20.0.9:8443", "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=", false},
		{"https://10.20.0.9:8443", "sha256/short", false},
		{"http://10.20.0.9:8443", pin, false},
	} {
		got, err := sandboxPin(func(k string) string {
			return map[string]string{"GUILDNET_SANDBOX_URL": c.url, "GUILDNET_SANDBOX_PIN": c.pin}[k]
		})
		if ok := err == nil; ok != c.ok || (ok && got != c.pin) {
			t.Errorf("%s with pin %q: %q, %v; want ok %v", c.url, c.pin, got, err, c.ok)
		}
	}
}

// serveBroker answers "ok" over TLS with the broker's certificate in dir.
func serveBroker(t *testing.T, dir string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(dir, "broker.crt"), filepath.Join(dir, "broker.key")))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "https://localhost:" + portOf(ln.Addr().String())
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}

// A self-hosted console keeps a CA of its own and gives its broker a
// certificate from it; a runner that trusts the CA's certificate, from where
// the console shares it, reaches the broker, and the share holds nothing
// else: no key.
func TestARunnerTrustingTheSharedCAReachesTheBroker(t *testing.T) {
	own, share := t.TempDir(), t.TempDir()
	if err := brokerCerts(own, share, []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	url := serveBroker(t, own)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientTLSFromFile(filepath.Join(share, "ca.crt"))}, Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("reaching the broker trusting the shared CA: %v", err)
	}
	resp.Body.Close()

	entries, _ := os.ReadDir(share)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"ca.crt"}) {
		t.Errorf("the share holds %v, want just ca.crt", names)
	}
}

// sandboxd, which has Docker's socket, is reached over TLS too: the console
// gives it a certificate from the same CA, in a folder it shares with
// sandboxd alone (the CA's key stays the console's), so the runner and the
// console reach it trusting that CA and nothing else.
func TestSandboxdIsReachedOverTLSFromTheConsolesCA(t *testing.T) {
	own, share, desks := t.TempDir(), t.TempDir(), t.TempDir()
	brokerCerts(own, share, []string{"localhost"}, time.Now())
	if err := sandboxCerts(own, desks, []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(desks, "sandboxd.crt"), filepath.Join(desks, "sandboxd.key")))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(ln)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientTLSFromFile(filepath.Join(share, "ca.crt"))}, Timeout: 5 * time.Second}
	resp, err := client.Get("https://localhost:" + portOf(ln.Addr().String()))
	if err != nil {
		t.Fatalf("reaching sandboxd trusting the console's CA: %v", err)
	}
	resp.Body.Close()
	entries, _ := os.ReadDir(desks)
	for _, e := range entries {
		if e.Name() == "ca.key" {
			t.Error("sandboxd's folder has the CA's key")
		}
	}
}

// sandboxd on a desks host of its own asks for its certificate (platform
// sandbox-cert, given its request): the console signs it from its CA for
// the names and addresses its settings give, and the runner reaches it
// there trusting the CA alone.
func TestADesksHostsSandboxdGetsItsCertificateFromTheConsole(t *testing.T) {
	own, share, host := t.TempDir(), t.TempDir(), t.TempDir()
	brokerCerts(own, share, []string{"platform"}, time.Now())
	csr, err := pki.Request(host, "sandboxd")
	if err != nil {
		t.Fatal(err)
	}
	var cert bytes.Buffer
	if err := signSandboxCert(own, []string{"sandboxd", "127.0.0.1"}, bytes.NewReader(csr), &cert, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := pki.Install(host, "sandboxd", cert.Bytes()); err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(host, "sandboxd.crt"), filepath.Join(host, "sandboxd.key")))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(ln)
	defer srv.Close()

	client := &http.Client{Transport: pki.ClientTransport(filepath.Join(share, "ca.crt")), Timeout: 5 * time.Second}
	resp, err := client.Get("https://127.0.0.1:" + portOf(ln.Addr().String()))
	if err != nil {
		t.Fatalf("reaching the desks host's sandboxd: %v", err)
	}
	resp.Body.Close()
	if err := signSandboxCert(own, []string{"sandboxd"}, strings.NewReader("not a request"), io.Discard, time.Now()); err == nil {
		t.Error("signed something that isn't a request")
	}
}

// Made again (each day, and each start) the CA stays the same, so runners
// keep trusting the broker.
func TestTheBrokersCAIsKept(t *testing.T) {
	own, share := t.TempDir(), t.TempDir()
	brokerCerts(own, share, []string{"localhost"}, time.Now())
	first, _ := os.ReadFile(filepath.Join(share, "ca.crt"))
	if err := brokerCerts(own, share, []string{"localhost"}, time.Now().AddDate(0, 11, 15)); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(share, "ca.crt")); string(again) != string(first) || len(first) == 0 {
		t.Error("the CA changed")
	}
}

// sandboxd over TLS is trusted by the CA its certificate is from: a
// self-hosted console's own (GUILDNET_BROKER_TLS_DIR), or, where a host
// holds GuildNet's CA (hosted), the one it shares with the console
// (GUILDNET_PKI_SHARED). With neither, the console can't check sandboxd.
func TestTheConsoleTrustsSandboxdByTheCAItsCertificateIsFrom(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"self-hosted", map[string]string{"GUILDNET_BROKER_TLS_DIR": "/broker"}, filepath.Join("/broker", "ca.crt")},
		{"hosted", map[string]string{"GUILDNET_PKI_SHARED": "/pki"}, filepath.Join("/pki", "ca.crt")},
		{"neither", map[string]string{}, ""},
	} {
		got, err := sandboxCA(func(k string) string { return c.env[k] })
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}
