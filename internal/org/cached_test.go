// SPDX-License-Identifier: AGPL-3.0-only

package org_test

import (
	"testing"

	"github.com/guildnet-chat/guildnet/internal/org"
)

// What a cached store hands out is the caller's to change: changing it
// changes nothing kept, until it's saved.
func TestChangingWhatTheCacheGaveChangesNothingKept(t *testing.T) {
	s := org.NewCached(org.NewMemory(org.Seed{
		Members: []org.Member{{Account: "dana", Access: []string{"Admin"}}},
		Teams:   []org.Team{{Name: "Sales", Channel: "#sales", Leads: []string{"dana"}}},
	}))

	m, _ := s.Member(ctx, "dana")
	m.Access[0] = "Owner"
	teams, _ := s.Teams(ctx)
	teams[0].Leads[0] = "mallory"
	team, _ := s.Team(ctx, "sales")
	team.Leads = append(team.Leads[:0], "eve")

	if m, _ := s.Member(ctx, "dana"); m.Access[0] != "Admin" {
		t.Errorf("dana's access: %v", m.Access)
	}
	if team, _ := s.Team(ctx, "Sales"); len(team.Leads) != 1 || team.Leads[0] != "dana" {
		t.Errorf("Sales' leads: %v", team.Leads)
	}
}

// A change made through the cache is seen at once, everywhere it reaches:
// someone removed is no longer a team's lead.
func TestAChangeThroughTheCacheIsSeenAtOnce(t *testing.T) {
	s := org.NewCached(org.NewMemory(org.Seed{
		Members: []org.Member{{Account: "dana"}},
		Teams:   []org.Team{{Name: "Sales", Channel: "#sales", Leads: []string{"dana"}}},
	}))
	s.Teams(ctx)
	s.Members(ctx)

	if err := s.RemoveMember(ctx, "dana"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Member(ctx, "dana"); err != org.ErrNotFound {
		t.Errorf("dana after removing: %v", err)
	}
	if team, _ := s.Team(ctx, "Sales"); team.LedBy("dana") {
		t.Error("dana still leads Sales")
	}
}
