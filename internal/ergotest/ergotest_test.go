// SPDX-License-Identifier: AGPL-3.0-only

package ergotest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/internal/ergotest"
)

// The message of the day (what IRC apps show on connecting) is about
// GuildNet: which network this is, how to work with agents, GuildServ, and
// where the web app is.
func TestNewcomersAreWelcomedToGuildNet(t *testing.T) {
	s, err := ergotest.Start(ergotest.WithWelcome("acme", "https://acme.guildnet.chat"))
	if errors.Is(err, ergotest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping real-Ergo tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	var mu sync.Mutex
	var motd []string
	done := make(chan struct{})
	c := &ircevent.Connection{Server: s.Addr, Nick: "newcomer", User: "newcomer"}
	c.AddCallback("372", func(e ircmsg.Message) { // RPL_MOTD
		mu.Lock()
		motd = append(motd, e.Params[len(e.Params)-1])
		mu.Unlock()
	})
	c.AddCallback("376", func(ircmsg.Message) { close(done) }) // RPL_ENDOFMOTD
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	go c.Loop()
	t.Cleanup(c.Quit)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no MOTD")
	}

	mu.Lock()
	text := strings.Join(motd, "\n")
	mu.Unlock()
	for _, want := range []string{"Welcome to acme on GuildNet", "/msg GuildServ HELP", "https://acme.guildnet.chat", "agents"} {
		if !strings.Contains(text, want) {
			t.Errorf("the MOTD should say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Ergo") || strings.Contains(text, "{{") {
		t.Errorf("the MOTD still has Ergo's default text or an unfilled blank:\n%s", text)
	}
}

// What the platform's invites and password resets ask of Ergo's API: a new
// account is refused for a name that's taken, and a password change takes
// effect at once, the old password no longer working.
func TestTheAPIMakesAccountsAndChangesPasswords(t *testing.T) {
	s, err := ergotest.Start()
	if errors.Is(err, ergotest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping real-Ergo tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	call := func(path string, body map[string]string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", s.APIURL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+s.APIToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	works := func(password string) bool {
		t.Helper()
		return call("/v1/check_auth", map[string]string{"accountName": "dee", "passphrase": password})["success"] == true
	}

	// A password can't have spaces, or start with a colon: the platform's
	// forms say so before asking.
	for _, bad := range []string{"has a space in it", "tab\tinside", ":starts-with-colon"} {
		if out := call("/v1/ns/saregister", map[string]string{"accountName": "dee", "passphrase": bad}); out["errorCode"] != "INVALID_PASSPHRASE" {
			t.Errorf("a password %q: %v, want INVALID_PASSPHRASE", bad, out)
		}
	}
	if out := call("/v1/ns/saregister", map[string]string{"accountName": "dee", "passphrase": "the-first-password"}); out["success"] != true {
		t.Fatalf("making dee: %v", out)
	}
	if out := call("/v1/ns/saregister", map[string]string{"accountName": "dee", "passphrase": "someone-elses"}); out["success"] == true || out["errorCode"] != "ACCOUNT_EXISTS" {
		t.Errorf("making dee again: %v, want ACCOUNT_EXISTS", out)
	}
	if out := call("/v1/ns/passwd", map[string]string{"accountName": "dee", "passphrase": "the-second-password"}); out["success"] != true {
		t.Fatalf("changing dee's password: %v", out)
	}
	if !works("the-second-password") || works("the-first-password") {
		t.Error("after the change, only the new password should work")
	}
	if out := call("/v1/ns/passwd", map[string]string{"accountName": "nobody", "passphrase": "whatever-it-is"}); out["success"] == true {
		t.Errorf("changing a password for no account: %v", out)
	}
}

func TestRegisteredAccountsCanLogIn(t *testing.T) {
	s, err := ergotest.Start()
	if errors.Is(err, ergotest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping real-Ergo tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)

	if err := s.Register("alice", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	loggedIn := make(chan string, 1)
	c := &ircevent.Connection{Server: s.Addr, Nick: "alice", User: "alice",
		UseSASL: true, SASLLogin: "alice", SASLPassword: "correct-horse-battery"}
	c.AddCallback("900", func(e ircmsg.Message) { loggedIn <- e.Params[2] })
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	go c.Loop()
	t.Cleanup(c.Quit)

	select {
	case account := <-loggedIn:
		if account != "alice" {
			t.Errorf("logged in as %q, want alice", account)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never logged in")
	}
}
