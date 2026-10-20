// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Ergo image's own config is closed when nothing overrides it: nobody
// registers an account for themselves, nobody connects without one, and
// there's no operator, API token or chat key anyone could know. A
// deployment that forgets a setting gets a server nobody can use, not one
// anybody can.
func TestErgosOwnConfigIsClosed(t *testing.T) {
	b, err := os.ReadFile("../ergo/ircd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Accounts struct {
			Registration struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"registration"`
			RequireSASL struct {
				Enabled  bool     `yaml:"enabled"`
				Exempted []string `yaml:"exempted"`
			} `yaml:"require-sasl"`
			JWT struct {
				Enabled bool `yaml:"enabled"`
				Tokens  []struct {
					Key string `yaml:"key"`
				} `yaml:"tokens"`
			} `yaml:"jwt-auth"`
		} `yaml:"accounts"`
		Server struct {
			ProxyAllowedFrom []string `yaml:"proxy-allowed-from"`
		} `yaml:"server"`
		Opers map[string]any `yaml:"opers"`
		API   struct {
			Enabled bool     `yaml:"enabled"`
			Tokens  []string `yaml:"bearer-tokens"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Accounts.Registration.Enabled {
		t.Error("anyone can register an account")
	}
	if !c.Accounts.RequireSASL.Enabled || len(c.Accounts.RequireSASL.Exempted) > 0 {
		t.Errorf("connecting without an account: required %v, except from %v", c.Accounts.RequireSASL.Enabled, c.Accounts.RequireSASL.Exempted)
	}
	// Who may say a visitor is someone else (PROXY, X-Forwarded-For): Ergo's
	// own container only, unless a deployment names its edge and console.
	if !slices.Equal(c.Server.ProxyAllowedFrom, []string{"localhost"}) {
		t.Errorf("PROXY is believed from %v, want just localhost", c.Server.ProxyAllowedFrom)
	}
	if len(c.Opers) > 0 {
		t.Errorf("operators %v are built in", keys(c.Opers))
	}
	if c.API.Enabled || len(c.API.Tokens) > 0 {
		t.Errorf("the API is on (%v) or has built-in tokens (%v)", c.API.Enabled, c.API.Tokens)
	}
	if c.Accounts.JWT.Enabled || slices.ContainsFunc(c.Accounts.JWT.Tokens, func(t struct {
		Key string `yaml:"key"`
	}) bool {
		return t.Key != ""
	}) {
		t.Error("chat tokens are on, or have a built-in key")
	}
}

// Someone flooding the network is slowed (Ergo's fakelag): after a burst,
// a few commands a second. The limits are well above what a person types
// or an agent says (Ergo bills a multiline reply by its size, about one
// command for every 512 bytes), and GuildServ, an operator, isn't slowed
// at all.
func TestFloodingIsSlowed(t *testing.T) {
	b, err := os.ReadFile("../ergo/ircd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Fakelag struct {
			Enabled  bool   `yaml:"enabled"`
			Window   string `yaml:"window"`
			Burst    int    `yaml:"burst-limit"`
			PerWin   int    `yaml:"messages-per-window"`
			Cooldown string `yaml:"cooldown"`
		} `yaml:"fakelag"`
		OperClasses map[string]struct {
			Extends      string   `yaml:"extends"`
			Capabilities []string `yaml:"capabilities"`
		} `yaml:"oper-classes"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	f := c.Fakelag
	if !f.Enabled {
		t.Fatal("fakelag is off: anyone can flood the network")
	}
	if f.Window != "1s" || f.Burst < 20 || f.PerWin < 4 || f.PerWin > 10 {
		t.Errorf("fakelag allows a burst of %d, then %d per %s: want a burst of 20 or more, then 4 to 10 a second", f.Burst, f.PerWin, f.Window)
	}
	exempt := func(class string) bool {
		for class != "" {
			oc := c.OperClasses[class]
			if slices.Contains(oc.Capabilities, "nofakelag") {
				return true
			}
			class = oc.Extends
		}
		return false
	}
	if !exempt("guildserv") {
		t.Error("GuildServ's operator class is slowed too")
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// GuildServ's operator class holds what it uses and nothing more: modes on
// the org's channels (samode), their registrations (chanreg), role
// hostnames (vhosts), reloading the welcome page (rehash), suspending the
// accounts of people taken out of the org (ban, which NickServ SUSPEND needs),
// and not being slowed.
// Not killing sessions, changing accounts, history, DEFCON, or messaging
// everyone. Every deployment opers it with that class.
func TestGuildServsOperClassHoldsOnlyWhatItUses(t *testing.T) {
	b, err := os.ReadFile("../ergo/ircd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		OperClasses map[string]struct {
			Extends      string   `yaml:"extends"`
			Capabilities []string `yaml:"capabilities"`
		} `yaml:"oper-classes"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	gs, ok := c.OperClasses["guildserv"]
	if !ok {
		t.Fatal("no guildserv operator class")
	}
	got := slices.Sorted(slices.Values(gs.Capabilities))
	if want := []string{"ban", "chanreg", "nofakelag", "rehash", "samode", "vhosts"}; gs.Extends != "" || !slices.Equal(got, want) {
		t.Errorf("guildserv extends %q and holds %v, want nothing extended and %v", gs.Extends, got, want)
	}
	for _, f := range []string{"compose.yml", "../gke/statefulset.yaml", "../aws-fargate/guildnet.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`ERGO__OPERS__GUILDSERV__CLASS\W+(?:value:\s*|Value:\s*)?([\w-]+)`).FindSubmatch(b)
		if m == nil || string(m[1]) != "guildserv" {
			t.Errorf("%s opers GuildServ as %q, want guildserv", f, m)
		}
	}
}
