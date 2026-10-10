// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/ircfake"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/runner"
)

// A network created from the small dev team template comes up with that
// template's agents in their channels, working for the network's owner.
func TestANetworkFromTheTeamTemplateComesUpWithItsTeam(t *testing.T) {
	srv, err := ircfake.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.AddAccount("ada", testPassword)

	t.Setenv("GUILDNET_OWNER", "ada")
	t.Setenv("GUILDNET_COMPANY", "acme")
	t.Setenv("GUILDNET_TIMEZONE", "Europe/Paris")
	t.Setenv("GUILDNET_DATA_DIR", t.TempDir())
	t.Setenv("GUILDNET_SERVER", srv.Addr())
	cfg, personas, err := config.Load(filepath.Join("..", "templates", "team", "guildnet.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(cfg.DataDir, cfg.DailyBudgetUSD)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	brain := &fakeBrain{reply: func(req agent.Request) string { return "on it" }}
	r := runner.New(cfg, org.NewMemory(org.SeedFrom(cfg, personas)), brain, l, log.New(io.Discard, "", 0))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)

	h := joinHumanLoggedIn(t, srv.Addr(), "ada", "ada", testPassword, "#general", "#engineering", "#product")
	n := &network{t: t, srv: srv, runner: r, brain: brain, human: h}
	n.waitForMembers("#engineering", sortedNicks("ada", "kit", "ledger", "pia", "rey")...)
	n.waitForMembers("#product", sortedNicks("ada", "ledger", "pia")...)

	h.say("#engineering", "rey: can you look over the login change?")
	h.expect("rey's answer", func(m heard) bool { return from("rey", "#engineering")(m) && m.text == "on it" })
	req := brain.lastRequest(t)
	p := req.Persona
	if p.Nick != "rey" || !strings.Contains(p.Prompt, "acme") || !strings.Contains(p.Prompt, "ada") || req.Location.String() != "Europe/Paris" {
		t.Errorf("%s woke with prompt %q in %s; want rey, working at acme for ada, on Paris time", p.Nick, p.Prompt, req.Location)
	}
}
