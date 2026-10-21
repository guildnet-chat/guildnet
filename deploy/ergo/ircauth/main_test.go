// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// guildnet-ircauth is Ergo's auth-script: Ergo hands it someone's login,
// it asks the console, and Ergo takes its answer as final (the patched,
// authoritative auth-script), except where it says to fall back to Ergo's
// own accounts (agents', which the console doesn't keep).

// console is a stand-in for the console's broker: it answers logins as
// answers says, and keeps what it was asked.
type console struct {
	srv     *httptest.Server
	answers map[string]string // password -> yes, no or not-mine
	asked   []map[string]string
	headers http.Header
}

func newConsole(t *testing.T) *console {
	t.Helper()
	c := &console{answers: map[string]string{}}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/irc/login" {
			http.NotFound(w, r)
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		c.asked = append(c.asked, in)
		c.headers = r.Header.Clone()
		answer := c.answers[in["password"]]
		if answer == "" {
			answer = "no"
		}
		json.NewEncoder(w).Encode(map[string]string{"answer": answer, "account": strings.ToLower(in["account"])})
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// env is Ergo's environment, as the host or Compose sets it, trusting the
// console's certificate.
func (c *console) env(t *testing.T) map[string]string {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw}), 0o644)
	return map[string]string{
		"GUILDNET_IRC_AUTH_URL":   c.srv.URL + "/irc/login",
		"GUILDNET_NETWORK":        "acme",
		"GUILDNET_ERGO_TOKEN":     "ergo-secret",
		"GUILDNET_BROKER_CA_FILE": ca,
	}
}

// login is Ergo running the script with one login on its input.
func login(t *testing.T, env map[string]string, input string) output {
	t.Helper()
	var out bytes.Buffer
	run(strings.NewReader(input+"\n"), &out, func(k string) string { return env[k] })
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("Ergo wants one line of JSON, got %q", out.String())
	}
	var o output
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("%q: %v", out.String(), err)
	}
	return o
}

func TestTheConsoleSaysWhoLogsIn(t *testing.T) {
	c := newConsole(t)
	c.answers["right"] = "yes"
	env := c.env(t)

	yes := login(t, env, `{"accountName":"Ada","passphrase":"right","ip":"203.0.113.9"}`)
	no := login(t, env, `{"accountName":"ada","passphrase":"wrong","ip":"203.0.113.9"}`)

	if !yes.Success || yes.AccountName != "ada" || yes.Fallback {
		t.Errorf("the right password: %+v", yes)
	}
	if no.Success || no.Fallback {
		t.Errorf("a wrong password: %+v, want a final no", no)
	}
	if got := c.asked[0]; got["account"] != "Ada" || got["password"] != "right" || got["ip"] != "203.0.113.9" {
		t.Errorf("the console was asked %v", got)
	}
	if c.headers.Get("Authorization") != "Bearer ergo-secret" || c.headers.Get("GuildNet-Network") != "acme" {
		t.Errorf("asked as %q for %q", c.headers.Get("Authorization"), c.headers.Get("GuildNet-Network"))
	}
}

// An account the console doesn't keep (an agent's) is Ergo's to check.
func TestAnAccountTheConsoleDoesntKeepIsErgos(t *testing.T) {
	c := newConsole(t)
	c.answers["agents"] = "not-mine"

	o := login(t, c.env(t), `{"accountName":"kai","passphrase":"agents","ip":"10.0.0.5"}`)

	if o.Success || !o.Fallback {
		t.Errorf("%+v, want Ergo to check its own", o)
	}
}

// Without the console, Ergo checks its own accounts: agents still connect;
// people's passwords aren't there, so they wait for the console.
func TestWithoutTheConsoleErgoChecksItsOwn(t *testing.T) {
	c := newConsole(t)
	env := c.env(t)
	c.srv.Close()

	o := login(t, env, `{"accountName":"kai","passphrase":"x"}`)

	if o.Success || !o.Fallback || o.Error == "" {
		t.Errorf("%+v, want a fallback and why", o)
	}
}

// Only a console with the CA's certificate is believed: anything else
// listening there can't say yes.
func TestOnlyTheConsoleIsBelieved(t *testing.T) {
	c := newConsole(t)
	c.answers["right"] = "yes"
	env := c.env(t)
	// Another CA's certificate (every httptest server has the same one).
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another CA"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	other := filepath.Join(t.TempDir(), "other.crt")
	os.WriteFile(other, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	env["GUILDNET_BROKER_CA_FILE"] = other

	if o := login(t, env, `{"accountName":"ada","passphrase":"right"}`); o.Success {
		t.Errorf("a server without the CA's certificate let ada in: %+v", o)
	}
}

// People's passwords go to the console over TLS or not at all: one named
// with http:// is never sent them.
func TestPasswordsNeverGoToTheConsoleInPlainHTTP(t *testing.T) {
	c := newConsole(t)
	heard := 0
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { heard++ }))
	t.Cleanup(plain.Close)
	env := c.env(t)
	env["GUILDNET_IRC_AUTH_URL"] = plain.URL + "/irc/login"

	o := login(t, env, `{"accountName":"ada","passphrase":"right"}`)

	if o.Success || heard != 0 {
		t.Errorf("%+v, and a plain-HTTP console was asked %d times", o, heard)
	}
}

// Logins without a password (a certificate's) aren't the console's: no.
func TestALoginWithoutAPasswordIsRefused(t *testing.T) {
	c := newConsole(t)

	o := login(t, c.env(t), `{"certfp":"abcd","ip":"10.0.0.5"}`)

	if o.Success || o.Fallback || len(c.asked) != 0 {
		t.Errorf("%+v (console asked %d times)", o, len(c.asked))
	}
}
