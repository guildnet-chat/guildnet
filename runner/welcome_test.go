// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/ergotest"
)

// IRC apps greet people with the network's welcome (MOTD), so it says what
// the company does and what it's aiming for, as the owner describes it, and
// keeps up when that changes.
func TestTheWelcomeSaysWhatTheCompanyIsFor(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{}, ergotest.WithWelcome("acme", "https://acme.guildnet.chat"))

	n.human.say("GuildServ", `COMPANY DESCRIBE "Bookkeeping for small bakeries. Costs $5 a month."`)
	n.human.expect("described", guildServSaying("method", "the company"))
	n.human.say("GuildServ", `GOAL ADD "Win 100 bakeries" MEASURE "Paying bakeries" TARGET 100 CURRENT 12`)
	n.human.expect("goal added", guildServSaying("method", "added"))
	n.human.say("GuildServ", `GOAL ADD "Tidy the docs" TEAM Engineering`)
	n.human.expect("objective added", guildServSaying("method", "added"))

	var motd string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		motd = readMOTD(t, n.human)
		if strings.Contains(motd, "Win 100 bakeries") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, want := range []string{"Welcome to acme on GuildNet", "Bookkeeping for small bakeries. Costs $5 a month.", "Win 100 bakeries (Paying bakeries: 12 of 100)", "/msg GuildServ HELP"} {
		if !strings.Contains(motd, want) {
			t.Errorf("the welcome should say %q:\n%s", want, motd)
		}
	}
	if strings.Contains(motd, "Tidy the docs") || strings.Contains(motd, "{{") {
		t.Errorf("the welcome has a team's objective or an unfilled blank:\n%s", motd)
	}
}

// The welcome's directory is shared with Ergo, which faces the internet: if
// it were taken over, it mustn't be able to point the runner at the
// runner's own files (its secrets) through a link, to have them written
// into the welcome for everyone to read.
func TestTheWelcomeNeverReadsOutsideItsDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "environ")
	os.WriteFile(secret, []byte("ANTHROPIC_API_KEY=sk-ant-secret\n{{COMPANY}}\n"), 0o600)
	if err := os.Symlink(secret, filepath.Join(dir, "base.motd")); err != nil {
		t.Skipf("can't make links here: %v", err)
	}
	n := startNetwork(t, &fakeBrain{}, func(cfg *config.Runner, _ *[]config.Persona) { cfg.WelcomeDir = dir })

	n.human.say("GuildServ", `COMPANY DESCRIBE "Bookkeeping for small bakeries."`)
	n.human.expect("described", guildServSaying("method", "the company"))
	time.Sleep(500 * time.Millisecond) // the welcome is written in the background

	if b, _ := os.ReadFile(filepath.Join(dir, "guildnet.motd")); strings.Contains(string(b), "sk-ant-secret") {
		t.Errorf("the welcome has what the link pointed at:\n%s", b)
	}
}

// What the welcome says of the company is made safe to show: one line at a
// time, with no control characters.
func TestTheWelcomeShowsTheDescriptionSafely(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "base.motd"), []byte("$bWelcome$b\n\n{{COMPANY}}\nThe rest.\n"), 0o644)
	n := startNetwork(t, &fakeBrain{}, func(cfg *config.Runner, _ *[]config.Persona) { cfg.WelcomeDir = dir })

	n.human.say("GuildServ", "COMPANY DESCRIBE \"Bookkeeping\x07 for bakeries.\x01\"")
	n.human.expect("described", guildServSaying("method", "the company"))
	var motd string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(filepath.Join(dir, "guildnet.motd"))
		if motd = string(b); strings.Contains(motd, "Bookkeeping") {
			break
		}
	}
	if !strings.Contains(motd, "Bookkeeping for bakeries.") || strings.ContainsAny(motd, "\x07\x01") || !strings.Contains(motd, "The rest.") {
		t.Errorf("the welcome = %q", motd)
	}
}

// readMOTD asks the server for its welcome, as IRC apps show it.
func readMOTD(t *testing.T, h *human) string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	done := make(chan struct{})
	var once sync.Once
	id1 := h.conn.AddCallback("372", func(e ircmsg.Message) { // RPL_MOTD
		mu.Lock()
		lines = append(lines, e.Params[len(e.Params)-1])
		mu.Unlock()
	})
	id2 := h.conn.AddCallback("376", func(ircmsg.Message) { once.Do(func() { close(done) }) }) // RPL_ENDOFMOTD
	defer h.conn.RemoveCallback(id1)
	defer h.conn.RemoveCallback(id2)
	h.conn.Send("MOTD")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no MOTD")
	}
	mu.Lock()
	defer mu.Unlock()
	return strings.Join(lines, "\n")
}
