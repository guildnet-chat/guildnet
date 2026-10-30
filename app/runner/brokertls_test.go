// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/pki"
)

// tlsBroker answers "ok" over TLS with a certificate for localhost from a
// new CA, whose certificate it returns.
func tlsBroker(t *testing.T) (url string, caPEM []byte) {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Issue(dir, "broker", []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(dir, "broker.crt"), filepath.Join(dir, "broker.key")))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	_, port, _ := strings.Cut(ln.Addr().String(), ":")
	return "https://localhost:" + port, ca.CertPEM()
}

// A self-hosted runner is given the console's CA as a file (shared with it
// by the console): it reaches a broker with a certificate from that CA, and
// not one from any other.
func TestTheRunnerTrustsTheBrokerCAItsGivenAsAFile(t *testing.T) {
	url, caPEM := tlsBroker(t)
	other, _ := tlsBroker(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := brokerClient(func(k string) string {
		return map[string]string{"GUILDNET_BROKER_CA_FILE": caFile}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("reaching the broker from the CA it's given: %v", err)
	}
	resp.Body.Close()
	if resp, err := client.Get(other); err == nil {
		resp.Body.Close()
		t.Error("reached a broker whose certificate is from another CA")
	}
}

// On guildnet.chat the host gives the CA in the runner's settings, as before.
func TestTheRunnerTrustsTheBrokerCAInItsSettings(t *testing.T) {
	url, caPEM := tlsBroker(t)
	client, err := brokerClient(func(k string) string {
		return map[string]string{"GUILDNET_BROKER_CA": b64(caPEM)}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("reaching the broker: %v", err)
	}
	resp.Body.Close()
}

// Given a CA, the runner reaches the broker over TLS or not at all: a broker
// named with http:// (a mistake in its settings, say) never sees the
// network's secret.
func TestWithACATheRunnerNeverCallsTheBrokerInPlainHTTP(t *testing.T) {
	_, caPEM := tlsBroker(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	heard := 0
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { heard++ }))
	t.Cleanup(plain.Close)
	for name, env := range map[string]map[string]string{
		"in its settings": {"GUILDNET_BROKER_CA": b64(caPEM)},
		"as a file":       {"GUILDNET_BROKER_CA_FILE": caFile},
	} {
		client, err := brokerClient(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("GET", plain.URL+"/model-keys", nil)
		req.Header.Set("Authorization", "Bearer the-network-secret")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("with the CA %s, the runner called %s", name, plain.URL)
		}
	}
	if heard != 0 {
		t.Errorf("a plain-HTTP broker was called %d times", heard)
	}
}

func TestTheRunnerRefusesACAThatIsntOne(t *testing.T) {
	if _, err := brokerClient(func(k string) string {
		return map[string]string{"GUILDNET_BROKER_CA": "bm90IGEgY2VydA=="}[k]
	}); err == nil {
		t.Error("took a CA that isn't a certificate")
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// The runner reaches sandboxd the way it reaches the broker, and holds it to
// the key the broker last said it has: another server with a certificate
// from the same CA for the same name (an old key, kept by whoever took a
// desks host) isn't sent anything.
func TestTheRunnerHoldsSandboxdToItsPinnedKey(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	serve := func() (url string, pin string) {
		out := t.TempDir()
		if err := ca.Issue(out, "sandboxd", []string{"localhost"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(out, "sandboxd.crt"), filepath.Join(out, "sandboxd.key")))
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		_, port, _ := strings.Cut(ln.Addr().String(), ":")
		pin, _ = pki.KeyPinFile(filepath.Join(out, "sandboxd.crt"))
		return "https://localhost:" + port, pin
	}
	real, pin := serve()
	old, _ := serve()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	os.WriteFile(caFile, ca.CertPEM(), 0o644)
	for name, env := range map[string]map[string]string{
		"in its settings": {"GUILDNET_BROKER_CA": b64(ca.CertPEM())},
		"as a file":       {"GUILDNET_BROKER_CA_FILE": caFile},
	} {
		broker, err := brokerClient(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: sandboxTransport(broker.Transport, func() string { return pin }), Timeout: 5 * time.Second}
		if resp, err := client.Get(real); err != nil {
			t.Errorf("with the CA %s, the pinned sandboxd: %v", name, err)
		} else {
			resp.Body.Close()
		}
		if resp, err := client.Get(old); err == nil {
			resp.Body.Close()
			t.Errorf("with the CA %s, reached a sandboxd with another key", name)
		}
	}
}
