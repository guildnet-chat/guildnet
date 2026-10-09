// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"strings"
	"testing"
)

// Hiring from the role catalogue: a preset fills in the title, model, daily
// cap, and a prompt to start from; anything said as well wins.

func TestHiringFromAPreset(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("hi")}
	n := startNetwork(t, brain)

	n.human.say("GuildServ", `HIRE pip PRESET support-agent TEAM Engineering`)
	n.human.expect("pip hired", guildServSaying("method", "hired pip (Support agent)"))
	n.human.say("GuildServ", `HIRE rey PRESET code-reviewer ROLE "Senior reviewer" MODEL claude-haiku-4-5`)
	n.human.expect("rey hired", guildServSaying("method", "hired rey (Senior reviewer)"))

	pip, _ := n.store.Agent(t.Context(), "pip")
	if pip.Model != "claude-haiku-4-5" || !strings.Contains(pip.Prompt, "support agent") || pip.DailyBudgetUSD != 1 {
		t.Errorf("pip = %+v, want the support agent preset", pip)
	}
	rey, _ := n.store.Agent(t.Context(), "rey")
	if rey.Role != "Senior reviewer" || rey.Model != "claude-haiku-4-5" || !strings.Contains(rey.Prompt, "code reviewer") {
		t.Errorf("rey = %+v, want the reviewer's prompt with the title and model given", rey)
	}
}

func TestAnUnknownPresetIsRefused(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	n.human.say("GuildServ", `HIRE pip PRESET astronaut`)

	n.human.expect("a refusal", guildServSaying("method", "ROLES"))
}

func TestTheCatalogueFromIRCAndTheAPI(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	n.human.say("GuildServ", "ROLES")
	n.human.expect("the departments", guildServSaying("method", "Support"))
	n.human.say("GuildServ", "ROLES Support")
	n.human.expect("Support's roles", guildServSaying("method", "support-agent"))

	status, out := n.api("method").call("GET", "/v1/roles", nil)
	if status != http.StatusOK || len(out["roles"].([]any)) < 20 {
		t.Errorf("GET /v1/roles: %d, %d roles", status, len(out["roles"].([]any)))
	}
	if status, out := n.api("method").call("POST", "/v1/staff", map[string]any{"nick": "bea", "preset": "bookkeeper"}); status != http.StatusCreated || out["role"] != "Bookkeeper" {
		t.Errorf("hiring a bookkeeper through the API: %d %v", status, out)
	}
}
