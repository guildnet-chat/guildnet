// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
)

// Team budgets: the owner gives each team a monthly budget; humans (at their
// monthly cost) and agents (at their daily cap x 30) count against it, and
// leads can only commit spend within what's left.

func TestOwnerGivesATeamAMonthlyBudget(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.say("GuildServ", "TEAM SET Sales BUDGET 500")
	n.human.expect("a confirmation", guildServSaying("method", "$500"))

	n.human.say("GuildServ", "BUDGET Sales")

	m := n.human.expect("the budget", guildServSaying("method", "Sales: "))
	for _, want := range []string{"$500.00/month", "$500.00 left"} {
		if !strings.Contains(m.text, want) {
			t.Errorf("BUDGET Sales = %q, want it to show %q", m.text, want)
		}
	}
}

// An agent outside GuildNet has a daily budget too, which counts against its team's, as any agent's does.
func TestAnOutsideAgentsBudgetCountsAgainstItsTeams(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.say("GuildServ", "TEAM SET Sales BUDGET 500")
	n.human.expect("a confirmation", guildServSaying("method", "$500"))

	n.human.say("GuildServ", `HIRE rex TEAM Sales PLATFORM outside BUDGET 10`)
	n.human.expect("rex hired", guildServSaying("method", "hired rex"))
	n.human.say("GuildServ", "BUDGET Sales")

	m := n.human.expect("the budget", guildServSaying("method", "Sales: "))
	if !strings.Contains(m.text, "rex") || strings.Contains(m.text, "$500.00 left") {
		t.Errorf("BUDGET Sales = %q, want rex's $10 a day counted", m.text)
	}
}

// The log says what an outside agent may spend: through the model gateway
// where the network has one; otherwise it calls models on keys of its own,
// and its budget is what it's planned to spend.
func TestTheLogSaysHowAnOutsideAgentSpends(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	n.human.say("GuildServ", `HIRE rex PLATFORM outside BUDGET 10`)
	n.human.expect("rex hired", guildServSaying("method", "hired rex"))

	entries, err := n.store.Log(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "hire" && e.Target == "rex" {
			if strings.Contains(e.Detail, "model gateway") || !strings.Contains(e.Detail, "$10.00/day") {
				t.Errorf("rex's hiring says %q: no gateway here, and $10.00/day", e.Detail)
			}
			return
		}
	}
	t.Fatalf("no hiring of rex in the log: %+v", entries)
}

func TestLeadHiresWithinTheirTeamsBudget(t *testing.T) {
	n, dana := orgWithSalesLedByDana(t, &fakeBrain{})

	dana.say("GuildServ", `HIRE rex ROLE "Account executive" BUDGET 5`)
	dana.expect("rex hired", guildServSaying("dana", "hired rex"))
	n.human.say("GuildServ", "BUDGET Sales")

	m := n.human.expect("the budget", guildServSaying("method", "Sales: "))
	for _, want := range []string{"rex $150.00", "$350.00 left"} {
		if !strings.Contains(m.text, want) {
			t.Errorf("BUDGET Sales = %q, want it to show %q (a $5/day agent commits $150/month)", m.text, want)
		}
	}
}

func TestLeadCantHirePastTheirTeamsBudget(t *testing.T) {
	_, dana := orgWithSalesLedByDana(t, &fakeBrain{})

	dana.say("GuildServ", `HIRE rex ROLE "Account executive" BUDGET 20`)

	m := dana.expect("a refusal", guildServSaying("dana", "left"))
	if !strings.Contains(m.text, "$500.00") || !strings.Contains(m.text, "$600.00") {
		t.Errorf("refusal %q should say what's left ($500) and what rex would cost ($600/month)", m.text)
	}
}

func TestHumanCostsCountAgainstTheTeamBudget(t *testing.T) {
	n, dana := orgWithSalesLedByDana(t, &fakeBrain{})
	n.human.say("GuildServ", "MEMBER SET dana COST 400")
	n.human.expect("cost set", guildServSaying("method", "updated dana"))

	dana.say("GuildServ", `HIRE rex ROLE "Account executive" BUDGET 5`)

	dana.expect("a refusal: only $100 left after dana's $400", guildServSaying("dana", "$100.00 left"))
}

func TestLeadCantRaiseAnAgentsBudgetPastTheTeam(t *testing.T) {
	_, dana := orgWithSalesLedByDana(t, &fakeBrain{})
	dana.say("GuildServ", `HIRE rex ROLE "Account executive" BUDGET 1`)
	dana.expect("rex hired", guildServSaying("dana", "hired rex"))

	dana.say("GuildServ", "SET rex BUDGET 20")

	dana.expect("a refusal", guildServSaying("dana", "left"))
}

func TestLeadCantCommitSpendInATeamWithNoBudget(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.human.say("GuildServ", "TEAM CREATE Sales")
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Sales`)
	n.human.say("GuildServ", "PROMOTE dana")
	n.human.expect("dana promoted", guildServSaying("method", "is now lead"))
	dana := n.joinAs("dana", "#general")

	dana.say("GuildServ", `HIRE rex ROLE "Account executive"`)

	dana.expect("a refusal", guildServSaying("dana", "no budget"))
}

func TestOwnerCanGoOverATeamBudgetButIsWarned(t *testing.T) {
	n, _ := orgWithSalesLedByDana(t, &fakeBrain{})

	n.human.say("GuildServ", `HIRE rex ROLE "Account executive" TEAM Sales BUDGET 20`)

	n.human.expect("rex hired", guildServSaying("method", "hired rex"))
	n.human.expect("a warning", guildServSaying("method", "over"))
}
