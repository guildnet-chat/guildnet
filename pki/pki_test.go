// SPDX-License-Identifier: AGPL-3.0-only

package pki_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/pki"
)

// GuildNet's own certificate authority, for the hops inside the VM: the
// host holds it, and issues certificates for its own API and the
// platform's broker; clients trust only it.

func TestTheCAIsMadeOnceAndKeptPrivate(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := pki.EnsureCA(dir)
	if err != nil || string(again.CertPEM()) != string(ca.CertPEM()) {
		t.Errorf("a second EnsureCA made a new CA: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(filepath.Join(dir, "ca.key")); info.Mode().Perm() != 0o600 {
			t.Errorf("the CA's key is %v, want 0600", info.Mode().Perm())
		}
	}
	block, _ := pem.Decode(ca.CertPEM())
	cert, _ := x509.ParseCertificate(block.Bytes)
	if !cert.IsCA || cert.NotAfter.Before(time.Now().AddDate(9, 0, 0)) {
		t.Errorf("CA: IsCA %v, until %v", cert.IsCA, cert.NotAfter)
	}
}

// serve answers "ok" over TLS with the server certificate in dir.
func serve(t *testing.T, certFile, keyFile string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(certFile, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).String()
}

func get(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestOnlyACertificateFromTheCAForTheRightNameIsTrusted(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	ca, _ := pki.EnsureCA(dir)
	if err := ca.Issue(out, "broker", []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, filepath.Join(out, "broker.crt"), filepath.Join(out, "broker.key"))
	_, port, _ := net.SplitHostPort(addr)
	trusting := func(caPEM []byte) *http.Client {
		cfg, err := pki.ClientTLS(caPEM)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	}

	if body, err := get(trusting(ca.CertPEM()), "https://localhost:"+port); err != nil || body != "ok" {
		t.Errorf("a client trusting the CA: %q, %v", body, err)
	}
	if _, err := get(trusting(ca.CertPEM()), "https://127.0.0.1:"+port); err == nil {
		t.Error("the certificate is for localhost, and was taken for 127.0.0.1")
	}
	other, _ := pki.EnsureCA(t.TempDir())
	if _, err := get(trusting(other.CertPEM()), "https://localhost:"+port); err == nil {
		t.Error("a client trusting another CA got through")
	}
	if _, err := get(&http.Client{Timeout: 5 * time.Second}, "https://localhost:"+port); err == nil {
		t.Error("a client trusting the usual roots got through")
	}
}

// A client can be set up before the CA's certificate exists (the platform
// starts beside the host that makes it): it's read at each connection.
func TestAClientReadsTheCAWhenItConnects(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	caFile := filepath.Join(out, "ca.crt")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientTLSFromFile(caFile)}, Timeout: 5 * time.Second}

	ca, _ := pki.EnsureCA(dir)
	ca.Issue(out, "host", []string{"localhost"}, time.Now())
	_, port, _ := net.SplitHostPort(serve(t, filepath.Join(out, "host.crt"), filepath.Join(out, "host.key")))
	if _, err := get(client, "https://localhost:"+port); err == nil {
		t.Error("connected with no CA to check against")
	}
	os.WriteFile(caFile, ca.CertPEM(), 0o644)
	if body, err := get(client, "https://localhost:"+port); err != nil || body != "ok" {
		t.Errorf("once the CA is there: %q, %v", body, err)
	}
}

// A server reached by its address (a desks host on a private network, with
// no name to reach it by) is trusted only if its certificate has that
// address: ClientTransport checks the address dialled. ClientTLSFromFile,
// which only sees the name the client sent (none, for an address), refuses
// it rather than check nothing.
func TestAServerReachedByAddressMustHaveIt(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	ca, _ := pki.EnsureCA(dir)
	caFile := filepath.Join(dir, "ca.crt")
	if err := ca.Issue(out, "sandboxd", []string{"sandboxd", "127.0.0.1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ca.Issue(out, "other", []string{"other", "10.9.9.9"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(serve(t, filepath.Join(out, "sandboxd.crt"), filepath.Join(out, "sandboxd.key")))
	_, otherPort, _ := net.SplitHostPort(serve(t, filepath.Join(out, "other.crt"), filepath.Join(out, "other.key")))
	transport := &http.Client{Transport: pki.ClientTransport(caFile), Timeout: 5 * time.Second}

	if body, err := get(transport, "https://127.0.0.1:"+port); err != nil || body != "ok" {
		t.Errorf("reached by its address: %q, %v", body, err)
	}
	if _, err := get(transport, "https://127.0.0.1:"+otherPort); err == nil {
		t.Error("a certificate for another address was taken for 127.0.0.1")
	}
	if _, err := get(transport, "https://localhost:"+port); err == nil {
		t.Error("a certificate for sandboxd and 127.0.0.1 was taken for localhost")
	}
	fromFile := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientTLSFromFile(caFile)}, Timeout: 5 * time.Second}
	if _, err := get(fromFile, "https://127.0.0.1:"+otherPort); err == nil {
		t.Error("ClientTLSFromFile took a server reached by address without checking it")
	}
}

// A server pinned by its key's hash is the only one trusted, even beside
// another with a good certificate from the same CA for the same address
// (sandboxd's old key, kept by whoever took a desks host). Checked as the
// connection's made, before anything is sent; the pin is asked for at each
// connection, so a new one is taken up; with none, the CA's word is enough.
func TestAPinnedServerIsTheOnlyOneTrusted(t *testing.T) {
	dir, real, old := t.TempDir(), t.TempDir(), t.TempDir()
	ca, _ := pki.EnsureCA(dir)
	caFile := filepath.Join(dir, "ca.crt")
	for _, out := range []string{real, old} {
		if err := ca.Issue(out, "sandboxd", []string{"127.0.0.1"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	_, port, _ := net.SplitHostPort(serve(t, filepath.Join(real, "sandboxd.crt"), filepath.Join(real, "sandboxd.key")))
	_, oldPort, _ := net.SplitHostPort(serve(t, filepath.Join(old, "sandboxd.crt"), filepath.Join(old, "sandboxd.key")))
	pin, err := pki.KeyPinFile(filepath.Join(real, "sandboxd.crt"))
	if err != nil || !strings.HasPrefix(pin, "sha256/") {
		t.Fatalf("pin %q, %v", pin, err)
	}
	var mu sync.Mutex
	current := pin
	client := &http.Client{Transport: pki.PinTransport(pki.ClientTransport(caFile), func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}), Timeout: 5 * time.Second}

	if body, err := get(client, "https://127.0.0.1:"+port); err != nil || body != "ok" {
		t.Errorf("the pinned server: %q, %v", body, err)
	}
	var sent atomic.Bool
	impostor := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent.Store(true) }))
	impostor.TLS = pki.ServerTLS(filepath.Join(old, "sandboxd.crt"), filepath.Join(old, "sandboxd.key"))
	impostor.StartTLS()
	t.Cleanup(impostor.Close)
	_, impostorPort, _ := net.SplitHostPort(strings.TrimPrefix(impostor.URL, "https://"))
	for _, p := range []string{oldPort, impostorPort} {
		if _, err := get(client, "https://127.0.0.1:"+p); err == nil {
			t.Error("a server with another key was trusted")
		}
	}
	if sent.Load() {
		t.Error("a request reached the server with another key")
	}
	// A new pin (sandboxd given a new key) is taken up at the next connection.
	mu.Lock()
	current, _ = pki.KeyPinFile(filepath.Join(old, "sandboxd.crt"))
	mu.Unlock()
	client.CloseIdleConnections()
	if _, err := get(client, "https://127.0.0.1:"+oldPort); err != nil {
		t.Errorf("after the pin changed: %v", err)
	}
	// No pin: the CA's word.
	mu.Lock()
	current = ""
	mu.Unlock()
	client.CloseIdleConnections()
	if _, err := get(client, "https://127.0.0.1:"+port); err != nil {
		t.Errorf("with no pin: %v", err)
	}
	// Plain transports (a CA given as settings) are pinned too.
	cfg, _ := pki.ClientTLS(ca.CertPEM())
	plain := &http.Client{Transport: pki.PinTransport(&http.Transport{TLSClientConfig: cfg}, func() string { return pin }), Timeout: 5 * time.Second}
	if _, err := get(plain, "https://127.0.0.1:"+port); err != nil {
		t.Errorf("a plain transport, the pinned server: %v", err)
	}
	if _, err := get(plain, "https://127.0.0.1:"+oldPort); err == nil {
		t.Error("a plain transport trusted a server with another key")
	}
}

// A server on another machine (a desks host) asks the CA for a certificate
// without its key leaving that machine: it makes a request, the CA signs it
// for the names and addresses the CA's side chooses (not those asked for),
// and the server installs the certificate beside its key. Asked again, it
// keeps its key.
func TestAServerElsewhereGetsACertificateItsKeyNeverLeaves(t *testing.T) {
	caDir, server := t.TempDir(), t.TempDir()
	ca, _ := pki.EnsureCA(caDir)
	csr, err := pki.Request(server, "sandboxd")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(csr), "PRIVATE KEY") {
		t.Fatal("the request has the key in it")
	}
	cert, err := ca.Sign(csr, "sandboxd", []string{"sandboxd", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.Install(server, "sandboxd", cert); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(serve(t, filepath.Join(server, "sandboxd.crt"), filepath.Join(server, "sandboxd.key")))
	client := &http.Client{Transport: pki.ClientTransport(filepath.Join(caDir, "ca.crt")), Timeout: 5 * time.Second}
	if body, err := get(client, "https://127.0.0.1:"+port); err != nil || body != "ok" {
		t.Errorf("the server with its signed certificate: %q, %v", body, err)
	}

	key, _ := os.ReadFile(filepath.Join(server, "sandboxd.key"))
	again, err := pki.Request(server, "sandboxd")
	if err != nil {
		t.Fatal(err)
	}
	if still, _ := os.ReadFile(filepath.Join(server, "sandboxd.key")); string(still) != string(key) {
		t.Error("asking again made a new key: the certificate being served no longer matches it")
	}
	if _, err := ca.Sign(again, "sandboxd", []string{"sandboxd"}, time.Now()); err != nil {
		t.Errorf("signing a second request: %v", err)
	}
}

// A certificate that isn't for the server's key isn't installed, and
// what isn't a request isn't signed.
func TestOnlyARealRequestIsSignedAndOnlyAMatchingCertificateInstalled(t *testing.T) {
	ca, _ := pki.EnsureCA(t.TempDir())
	server, other := t.TempDir(), t.TempDir()
	pki.Request(server, "sandboxd")
	csr, _ := pki.Request(other, "sandboxd")
	cert, _ := ca.Sign(csr, "sandboxd", []string{"sandboxd"}, time.Now())
	if err := pki.Install(server, "sandboxd", cert); err == nil {
		t.Error("installed a certificate for another key")
	}
	if _, err := os.Stat(filepath.Join(server, "sandboxd.crt")); err == nil {
		t.Error("the mismatched certificate was written")
	}
	// A request whose signature (at its end) was changed.
	block, _ := pem.Decode(csr)
	block.Bytes[len(block.Bytes)-1] ^= 0xff
	tampered := string(pem.EncodeToMemory(block))
	for _, bad := range []string{"", "not a request", tampered, string(cert)} {
		if _, err := ca.Sign([]byte(bad), "sandboxd", []string{"sandboxd"}, time.Now()); err == nil {
			t.Errorf("signed %.20q", bad)
		}
	}
}

// Certificates last a year and are issued again when they're near their
// end; a server picks the new one up without restarting.
func TestCertificatesAreRenewedBeforeTheyRunOut(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	ca, _ := pki.EnsureCA(dir)
	now := time.Now()
	ca.Issue(out, "broker", []string{"localhost"}, now)
	first, _ := os.ReadFile(filepath.Join(out, "broker.crt"))

	ca.Issue(out, "broker", []string{"localhost"}, now.AddDate(0, 6, 0))
	if same, _ := os.ReadFile(filepath.Join(out, "broker.crt")); string(same) != string(first) {
		t.Error("renewed with half a year left")
	}
	ca.Issue(out, "broker", []string{"localhost"}, now.AddDate(0, 11, 10))
	if renewed, _ := os.ReadFile(filepath.Join(out, "broker.crt")); string(renewed) == string(first) {
		t.Error("not renewed with under a month left")
	}
	// Other names on it (the broker moved) are issued again too.
	ca.Issue(out, "broker", []string{"localhost", "platform"}, now.AddDate(0, 11, 10))
	b, _ := os.ReadFile(filepath.Join(out, "broker.crt"))
	block, _ := pem.Decode(b)
	cert, _ := x509.ParseCertificate(block.Bytes)
	if len(cert.DNSNames) != 2 {
		t.Errorf("names %v", cert.DNSNames)
	}
	// And so is one whose address changed.
	ca.Issue(out, "broker", []string{"localhost", "platform", "10.0.0.5"}, now.AddDate(0, 11, 10))
	ca.Issue(out, "broker", []string{"localhost", "platform", "10.0.0.6"}, now.AddDate(0, 11, 10))
	b, _ = os.ReadFile(filepath.Join(out, "broker.crt"))
	block, _ = pem.Decode(b)
	cert, _ = x509.ParseCertificate(block.Bytes)
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.6" || len(cert.DNSNames) != 2 {
		t.Errorf("names %v, addresses %v", cert.DNSNames, cert.IPAddresses)
	}
}
