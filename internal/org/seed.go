// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"strings"
	"unicode"

	"github.com/guildnet-chat/guildnet/config"
)

// SeedFrom builds the starting org chart from the runner config and the
// persona files. The owner is always a member, even if the config leaves
// them out.
func SeedFrom(cfg *config.Runner, agents []config.Persona) Seed {
	s := Seed{Agents: agents, Company: strings.TrimSpace(cfg.Description)}
	for _, g := range cfg.Goals {
		s.Goals = append(s.Goals, Goal{Team: g.Team, Text: g.Text, Measure: g.Measure, Target: g.Target, Current: g.Current})
	}
	for _, l := range cfg.Teams {
		ch := l.Channel
		if ch == "" {
			ch = TeamChannel(l.Name)
		}
		t := Team{Name: l.Name, Channel: ch, Parent: l.Parent, Focus: l.Focus, Private: l.Private, MonthlyBudget: l.Budget}
		if l.Lead != "" {
			t.Leads = []string{l.Lead}
		}
		s.Teams = append(s.Teams, t)
	}
	haveOwner := false
	for _, m := range cfg.Members {
		s.Members = append(s.Members, Member{Account: m.Account, Name: m.Name, Role: m.Role,
			Team: m.Team, ReportsTo: m.ReportsTo, Guest: m.Guest, MonthlyCost: m.Cost})
		haveOwner = haveOwner || strings.EqualFold(m.Account, cfg.Owner)
	}
	if !haveOwner && cfg.Owner != "" {
		s.Members = append(s.Members, Member{Account: cfg.Owner, Role: "Owner"})
	}
	return s
}

// TeamChannel is the default channel for a team: "Customer Success" becomes
// "#customer-success", and "R&D, Labs" "#r-d-labs". Only letters, digits,
// _ and . are kept, the rest become dashes: a channel's name is one word.
func TeamChannel(team string) string {
	words := strings.FieldsFunc(strings.ToLower(team), func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '.'
	})
	name := []rune(strings.Join(words, "-"))
	if len(name) > 50 {
		name = []rune(strings.TrimRight(string(name[:50]), "-"))
	}
	if len(name) == 0 {
		return "#team"
	}
	return "#" + string(name)
}
