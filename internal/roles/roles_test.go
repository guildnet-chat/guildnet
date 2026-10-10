// SPDX-License-Identifier: AGPL-3.0-only

package roles_test

import (
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/roles"
	"github.com/guildnet-chat/guildnet/models"
)

// The role catalogue: presets a company can hire from, by department. Each
// is a starting point, changed freely once hired.

func TestEveryRoleIsReadyToHire(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range roles.All {
		if r.Key == "" || r.Title == "" || r.Department == "" || r.About == "" || strings.TrimSpace(r.Prompt) == "" {
			t.Errorf("%q is missing something: %+v", r.Key, r)
		}
		if seen[r.Key] {
			t.Errorf("%s is in the catalogue twice", r.Key)
		}
		seen[r.Key] = true
		// A tier, not a model: every preset works whatever providers a
		// company has; new hires start on fast or balanced, never the dearest.
		if r.Tier != models.Fast && r.Tier != models.Balanced {
			t.Errorf("%s starts on tier %q; presets start on fast or balanced", r.Key, r.Tier)
		}
		if r.DailyBudget <= 0 || r.DailyBudget > 3 {
			t.Errorf("%s's daily cap is $%.2f; keep a new hire's spend small", r.Key, r.DailyBudget)
		}
		if len(r.AsksFirst) == 0 {
			t.Errorf("%s says nothing about what it asks before doing", r.Key)
		}
		if strings.Contains(r.Prompt, "{{") {
			t.Errorf("%s's prompt has a placeholder: presets are about the company, not a name", r.Key)
		}
	}
	if len(roles.All) < 20 {
		t.Errorf("only %d roles; the catalogue covers every department of a small company", len(roles.All))
	}
}

func TestRolesAreFoundByKeyOrTitle(t *testing.T) {
	for _, name := range []string{"support-agent", "Support agent", "SUPPORT AGENT"} {
		if r, ok := roles.Find(name); !ok || r.Key != "support-agent" {
			t.Errorf("Find(%q) = %v, %v", name, r.Key, ok)
		}
	}
	if _, ok := roles.Find("astronaut"); ok {
		t.Error("found a role that isn't in the catalogue")
	}
}

func TestTheCatalogueCoversEveryDepartment(t *testing.T) {
	got := map[string]bool{}
	for _, d := range roles.Departments() {
		got[d] = true
	}
	for _, want := range []string{"Leadership", "Engineering", "Platform", "Security", "Product & design", "Data", "Support", "Sales", "Marketing", "Operations"} {
		if !got[want] {
			t.Errorf("no roles in %s", want)
		}
	}
}
