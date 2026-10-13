// SPDX-License-Identifier: AGPL-3.0-only

package ergotest_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/internal/ergotest"
)

// GuildNet's Ergo asks the console about logins (guildnet-ircauth), and the
// console's answer is final: people's passwords are the console's, so its
// lockout holds on IRC too, and a password left in Ergo is no way around
// it. Agents' accounts, which the console doesn't keep, are Ergo's. Over
// IRC, people can't change their password, add a certificate or unregister.

// fakeConsole answers logins, by password: yes, no or not-mine.
type fakeConsole struct {
	mu      sync.Mutex
	answers map[string]string
	asked   int
}

func (c *fakeConsole) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/irc/login" || r.Header.Get("Authorization") != "Bearer ergo-token" || r.Header.Get("GuildNet-Network") != "acme" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	var in map[string]string
	json.NewDecoder(r.Body).Decode(&in)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	answer := c.answers[in["password"]]
	if answer == "" {
		answer = "no"
	}
	json.NewEncoder(w).Encode(map[string]string{"answer": answer, "account": strings.ToLower(in["account"])})
}

// startWithConsole is GuildNet's Ergo asking a console on the Docker host.
func startWithConsole(t *testing.T, answers map[string]string) (*ergotest.Server, *fakeConsole) {
	t.Helper()
	ca, caPEM, cert := consoleCerts(t)
	_ = ca
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeConsole{answers: answers}
	srv := &http.Server{Handler: c, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	go srv.ServeTLS(l, "", "")
	t.Cleanup(func() { srv.Close() })
	url := fmt.Sprintf("https://host.docker.internal:%d/irc/login", l.Addr().(*net.TCPAddr).Port)
	s, err := ergotest.Start(ergotest.WithConsole(url, caPEM, "acme", "ergo-token"))
	if errors.Is(err, ergotest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping real-Ergo tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s, c
}

// consoleCerts is a CA, and a certificate from it for the console as Ergo
// reaches it.
func consoleCerts(t *testing.T) (*x509.Certificate, []byte, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "console"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"host.docker.internal"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	return ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// register makes an account through the API, as the console (no password:
// the console keeps it) or the runner (an agent's) does.
func register(t *testing.T, s *ergotest.Server, account, password string) {
	t.Helper()
	if err := s.Register(account, password); err != nil {
		t.Fatal(err)
	}
}

func TestTheConsoleSaysWhoSignsInOverIRC(t *testing.T) {
	s, _ := startWithConsole(t, map[string]string{"adas-password": "yes"})
	register(t, s, "ada", "")

	right, err := s.SignsIn("ada", "adas-password")
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := s.SignsIn("ada", "a-guess")

	if !right || wrong {
		t.Errorf("ada with her password: %v; with a guess: %v", right, wrong)
	}
}

// A password left in Ergo (from before, or set some other way) doesn't get
// past the console's no.
func TestTheConsolesNoIsFinal(t *testing.T) {
	s, _ := startWithConsole(t, map[string]string{})
	register(t, s, "eve", "left-in-ergo")

	if in, _ := s.SignsIn("eve", "left-in-ergo"); in {
		t.Error("Ergo's own password got past the console's no")
	}
}

func TestAgentsAccountsAreErgos(t *testing.T) {
	s, _ := startWithConsole(t, map[string]string{"kais-secret": "not-mine", "a-guess": "not-mine"})
	register(t, s, "kai", "kais-secret")

	right, err := s.SignsIn("kai", "kais-secret")
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := s.SignsIn("kai", "a-guess")

	if !right || wrong {
		t.Errorf("kai with its password: %v; with a guess: %v", right, wrong)
	}
}

// Over IRC, someone with only ada's password can't make it theirs: no new
// password, no certificate, and the account can't be deleted.
func TestPeopleCantChangeTheirCredentialsOverIRC(t *testing.T) {
	s, _ := startWithConsole(t, map[string]string{"adas-password": "yes"})
	register(t, s, "ada", "")
	var mu sync.Mutex
	var notices []string
	ready := make(chan struct{})
	c := &ircevent.Connection{Server: s.Addr, Nick: "ada", User: "ada", UseSASL: true, SASLLogin: "ada", SASLPassword: "adas-password", RequestCaps: []string{"sasl"}}
	c.AddCallback("NOTICE", func(e ircmsg.Message) {
		if strings.EqualFold(e.Nick(), "NickServ") {
			mu.Lock()
			notices = append(notices, e.Params[len(e.Params)-1])
			mu.Unlock()
		}
	})
	c.AddConnectCallback(func(ircmsg.Message) { close(ready) })
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	go c.Loop()
	t.Cleanup(c.Quit)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("ada didn't connect")
	}

	for _, line := range []string{
		"NS PASSWD adas-password mine-now mine-now",
		"NS CERT ADD 0000000000000000000000000000000000000000000000000000000000000000",
		"NS UNREGISTER ada",
	} {
		mu.Lock()
		before := len(notices)
		mu.Unlock()
		c.SendRaw(line)
		var said string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			mu.Lock()
			if len(notices) > before {
				said = strings.Join(notices[before:], " ")
			}
			mu.Unlock()
			if said != "" {
				break
			}
		}
		if !strings.Contains(said, "managed externally") {
			t.Errorf("%s: NickServ said %q", line, said)
		}
	}
	if in, _ := s.SignsIn("ada", "mine-now"); in {
		t.Error("the password set over IRC signs in")
	}
}
