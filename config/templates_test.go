// SPDX-License-Identifier: AGPL-3.0-only

package config_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/templates"
)

// loadTemplate loads a template the way a new network's runner does: the
// owner, company and timezone come from the environment.
func loadTemplate(t *testing.T, name string) (*config.Runner, []config.Persona) {
	t.Helper()
	t.Setenv("GUILDNET_OWNER", "ada")
	t.Setenv("GUILDNET_COMPANY", "acme")
	t.Setenv("GUILDNET_TIMEZONE", "Europe/Paris")
	t.Setenv("GUILDNET_DATA_DIR", t.TempDir())
	cfg, personas, err := config.Load(filepath.Join("..", "templates", name, "guildnet.yaml"))
	if err != nil {
		t.Fatalf("template %s: %v", name, err)
	}
	return cfg, personas
}

func TestEveryTemplateIsForTheNetworksOwnerCompanyAndTimezone(t *testing.T) {
	for _, tpl := range templates.All {
		name, who := tpl.Name, tpl.Title
		t.Run(name, func(t *testing.T) {
			cfg, personas := loadTemplate(t, name)
			if cfg.Owner != "ada" {
				t.Errorf("the template for %s is owned by %q, want the network's owner ada", who, cfg.Owner)
			}
			if cfg.Location.String() != "Europe/Paris" {
				t.Errorf("timezone = %s, want the network's Europe/Paris", cfg.Location)
			}
			if !strings.Contains(cfg.Company, "acme") {
				t.Errorf("the company description doesn't name the company:\n%s", cfg.Company)
			}
			texts := []string{cfg.Company, cfg.Owner}
			for _, l := range cfg.Teams {
				texts = append(texts, l.Lead)
			}
			for _, p := range personas {
				texts = append(texts, p.Prompt, p.ReportsTo, p.Name)
			}
			for _, s := range texts {
				if strings.Contains(s, "{{") {
					t.Errorf("template text isn't about the new network: %q", s)
				}
			}
		})
	}
}

func TestEveryTemplatesOrgChartHangsTogether(t *testing.T) {
	for _, tpl := range templates.All {
		name := tpl.Name
		t.Run(name, func(t *testing.T) {
			cfg, personas := loadTemplate(t, name)
			people := map[string]bool{"ada": true}
			for _, m := range cfg.Members {
				people[strings.ToLower(m.Account)] = true
			}
			for _, p := range personas {
				people[strings.ToLower(p.Nick)] = true
			}
			teams := map[string]bool{}
			for _, l := range cfg.Teams {
				teams[l.Name] = true
				if !people[strings.ToLower(l.Lead)] {
					t.Errorf("team %s is led by %q, who isn't in the network", l.Name, l.Lead)
				}
			}
			if len(personas) == 0 {
				t.Fatal("the template has no agents")
			}
			for _, p := range personas {
				if !teams[p.Team] {
					t.Errorf("agent %s is in team %q, which the template doesn't have", p.Nick, p.Team)
				}
				if !people[strings.ToLower(p.ReportsTo)] {
					t.Errorf("agent %s reports to %q, who isn't in the network", p.Nick, p.ReportsTo)
				}
				if !slices.Contains(p.Channels, "#general") {
					t.Errorf("agent %s isn't in #general", p.Nick)
				}
				if p.Model != "claude-haiku-4-5" {
					t.Errorf("agent %s starts on %s; templates start everyone on Haiku to keep it cheap", p.Nick, p.Model)
				}
				if strings.TrimSpace(p.Prompt) == "" || p.Role == "" {
					t.Errorf("agent %s needs a role and a prompt", p.Nick)
				}
			}
			if !strings.Contains(cfg.Description, "acme") || len(cfg.Goals) == 0 {
				t.Errorf("the template should start the company's description (naming it) and a goal or two: %q, %v", cfg.Description, cfg.Goals)
			}
			for _, g := range cfg.Goals {
				if g.Team != "" && !teams[g.Team] {
					t.Errorf("goal %q is for team %q, which the template doesn't have", g.Text, g.Team)
				}
			}
			for _, l := range cfg.Teams {
				if l.Focus == "" {
					t.Errorf("team %s has no focus", l.Name)
				}
				if l.Parent != "" && !teams[l.Parent] {
					t.Errorf("team %s is inside %q, which the template doesn't have", l.Name, l.Parent)
				}
			}
			if cfg.DailyBudgetUSD > 5 {
				t.Errorf("the network's daily cap starts at $%.2f; keep a new network's spend small", cfg.DailyBudgetUSD)
			}
		})
	}
}

func TestTheTemplatesAgentsAreWhoTheyreFor(t *testing.T) {
	roles := func(ps []config.Persona) string {
		var out []string
		for _, p := range ps {
			out = append(out, strings.ToLower(p.Role))
		}
		return strings.Join(out, "; ")
	}
	_, solo := loadTemplate(t, "solo")
	if len(solo) != 1 {
		t.Errorf("a solo developer gets one generalist agent, not %d", len(solo))
	}
	_, team := loadTemplate(t, "team")
	for _, want := range []string{"engineer", "review", "product"} {
		if !strings.Contains(roles(team), want) {
			t.Errorf("a small dev team's agents (%s) have no %s", roles(team), want)
		}
	}
	for name, wants := range map[string][]string{
		"saas":     {"chief of staff", "engineer", "review", "support", "content"},
		"agency":   {"engineer", "design", "account", "bookkeep"},
		"smallbiz": {"support", "content", "community", "bookkeep"},
	} {
		_, ps := loadTemplate(t, name)
		for _, want := range wants {
			if !strings.Contains(roles(ps), want) {
				t.Errorf("the %s template's agents (%s) have no %s", name, roles(ps), want)
			}
		}
	}
	_, oss := loadTemplate(t, "opensource")
	for _, want := range []string{"triage", "doc"} {
		if !strings.Contains(roles(oss), want) {
			t.Errorf("an open-source project's agents (%s) have no %s", roles(oss), want)
		}
	}
}

func TestANetworksRunnerKeepsItsDataWhereItsToldTo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GUILDNET_DATA_DIR", dir)
	cfg, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != dir {
		t.Errorf("data dir = %s, want GUILDNET_DATA_DIR's %s", cfg.DataDir, dir)
	}
}

func TestAnOwnerFromTheEnvironmentMustBeAnIRCNick(t *testing.T) {
	for _, bad := range []string{"ada lovelace", "ada\nowner: eve", "#ada", "{{owner}}"} {
		t.Setenv("GUILDNET_OWNER", bad)
		if _, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667")); err == nil {
			t.Errorf("owner %q was accepted", bad)
		}
	}
}
