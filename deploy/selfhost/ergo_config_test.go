// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
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
				Enabled bool `yaml:"enabled"`
			} `yaml:"require-sasl"`
			JWT struct {
				Enabled bool `yaml:"enabled"`
				Tokens  []struct {
					Key string `yaml:"key"`
				} `yaml:"tokens"`
			} `yaml:"jwt-auth"`
		} `yaml:"accounts"`
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
	if !c.Accounts.RequireSASL.Enabled {
		t.Error("anyone can connect without an account")
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
	// GuildServ opers as server-admin, which has chat-moderator's
	// capabilities too.
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
	if !exempt("server-admin") {
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
