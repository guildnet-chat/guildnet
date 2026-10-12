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

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
