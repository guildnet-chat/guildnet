// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Agents at work: tool steps show in IRC as actions and in the away
// message, what the platform charges counts toward budgets, and staffing
// changes reach the agent platform.

// working makes kai take some steps before replying.
func working(steps ...string) *fakeBrain {
	return &fakeBrain{
		reply: replyWith("tests pass"),
		steps: func(agent.Request) []string { return steps },
	}
}

func actionFrom(nick, to, substr string) func(heard) bool {
	return func(m heard) bool { return m.action && fromContaining(nick, to, substr)(m) }
}

// By default a channel sees a summary of an agent's work, not each step:
// that it's working (at its first step), what it achieved, and a closing
// line the runner writes from the steps. The steps themselves are its away
// message as it goes.
func TestAChannelSeesASummaryOfTheWorkNotEachStep(t *testing.T) {
	n := startNetwork(t, working("runs go test ./...", "reads a.go", "reads b.go", "uses Linear: list_issues (limit: 5)", "=> opened PR #3: https://github.com/x/pull/3"))

	n.human.say("#engineering", "kai: open a PR")

	n.human.expect("kai says it's working", actionFrom("kai", "#engineering", "is working on it"))
	n.human.expect("what it achieved", actionFrom("kai", "#engineering", "opened PR #3"))
	summary := n.human.expect("the summary", actionFrom("kai", "#engineering", "4 steps"))
	for _, want := range []string{"worked ", "reads 2", "runs 1", "uses Linear 1"} {
		if !strings.Contains(summary.text, want) {
			t.Errorf("summary %q should have %q", summary.text, want)
		}
	}
	if strings.Contains(summary.text, "$") {
		t.Errorf("summary %q shouldn't say what it cost: not everyone in the channel may see spend", summary.text)
	}
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests pass"))
	for _, m := range n.human.all() {
		if m.action && m.from == "kai" && (strings.Contains(m.text, "go test") || strings.Contains(m.text, "a.go")) {
			t.Errorf("a step was posted: %q", m.text)
		}
	}
}

// A team can have every step shown, as it happens (a few a second at
// most), or only what agents achieve.
func TestATeamChoosesHowMuchOfTheWorkItsChannelSees(t *testing.T) {
	n := startNetwork(t, working("runs go test ./...", "=> opened PR #3"))
	n.human.say("GuildServ", "TEAM SET Engineering CHATTER steps")
	n.human.expect("chatter set", guildServSaying("method", "chatter steps"))

	n.human.say("#engineering", "kai: run the tests")
	n.human.expect("kai's step as an action", actionFrom("kai", "#engineering", "runs go test ./..."))
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests pass"))

	n.human.say("GuildServ", "TEAM SET Engineering CHATTER quiet")
	n.human.expect("chatter set", guildServSaying("method", "chatter quiet"))
	n.human.say("#engineering", "kai: again")
	n.human.expect("the result, even so", actionFrom("kai", "#engineering", "opened PR #3"))
	n.human.expectQuiet("anything else", 300*time.Millisecond, func(m heard) bool {
		return m.action && m.from == "kai" && (strings.Contains(m.text, "working on it") || strings.Contains(m.text, "steps"))
	})
	n.human.say("GuildServ", "TEAM SET Engineering CHATTER loud")
	n.human.expect("refused", guildServSaying("method", "quiet, summary or steps"))
}

func TestWorkForADMStaysInTheDM(t *testing.T) {
	n := startNetwork(t, working("reads config.go"))

	n.human.say("kai", "what's in the config?")

	n.human.expect("kai working, in the DM", actionFrom("kai", "method", "is working on it"))
	n.human.expect("the summary, in the DM", actionFrom("kai", "method", "1 step"))
	n.human.expectQuiet("anything in a channel", 300*time.Millisecond, func(m heard) bool {
		return m.action && strings.HasPrefix(m.to, "#")
	})
}

func TestTheAwayMessageSaysWhatTheAgentIsDoing(t *testing.T) {
	brain, release := slowBrain()
	brain.steps = func(agent.Request) []string { return []string{"runs go test ./..."} }
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai away with its current step", func(m heard) bool {
		return awayFrom("kai")(m) && strings.Contains(m.text, "go test")
	})
	close(release)
	n.human.expect("kai back", backFrom("kai"))
}

func TestABurstOfStepsDoesntFloodTheChannel(t *testing.T) {
	var steps []string
	for i := 0; i < 20; i++ {
		steps = append(steps, "edits file")
	}
	n := startNetwork(t, working(steps...))
	n.human.say("GuildServ", "TEAM SET Engineering CHATTER steps")
	n.human.expect("chatter set", guildServSaying("method", "chatter steps"))

	n.human.say("#engineering", "kai: refactor everything")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests pass"))
	time.Sleep(200 * time.Millisecond)

	actions := 0
	for _, m := range n.human.all() {
		if m.action && m.from == "kai" {
			actions++
		}
	}
	if actions == 0 || actions > 3 {
		t.Errorf("20 quick steps posted %d actions; want a few at most", actions)
	}
}

func TestWhatThePlatformChargesCountsTowardBudgets(t *testing.T) {
	brain := working("runs the build")
	brain.cost = 3.25
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: build it")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests pass"))
	n.human.say("#engineering", "!spend")

	m := n.human.expect("the spend report", fromContaining("ledger", "#engineering", "spend"))
	if !strings.Contains(m.text, "kai $3.250") {
		t.Errorf("spend report %q should charge kai the platform's $3.25", m.text)
	}
}

func TestStaffingChangesReachTheAgentPlatform(t *testing.T) {
	brain := &fakeBrain{}
	n := startNetwork(t, brain)

	n.human.say("GuildServ", `HIRE rex ROLE "Support" TEAM Engineering`)
	n.human.expect("rex hired", guildServSaying("method", "hired rex"))
	n.human.say("GuildServ", "SET rex MODEL claude-opus-5")
	n.human.expect("rex changed", guildServSaying("method", "rex's model"))
	n.human.say("GuildServ", "FIRE rex")
	n.human.expect("rex fired", guildServSaying("method", "fired rex"))

	brain.mu.Lock()
	defer brain.mu.Unlock()
	want := []string{"hire rex", "update rex claude-opus-5", "fire rex"}
	if strings.Join(brain.staffing, ", ") != strings.Join(want, ", ") {
		t.Errorf("the platform heard %v, want %v", brain.staffing, want)
	}
}

func TestWhatAToolAchievedIsAlwaysShownEvenAmongManyQuickSteps(t *testing.T) {
	n := startNetwork(t, working("runs git status", "reads a.go", "reads b.go", "=> opened PR #3: https://github.com/x/pull/3"))

	n.human.say("#engineering", "kai: open a PR")

	n.human.expect("the result, not folded into skipped steps", actionFrom("kai", "#engineering", "opened PR #3"))
}

// When a wake-up fails, #ops hears why, whatever the error looks like (a
// provider's can run over several lines), and whoever asked is told the
// agent couldn't answer, so it isn't just silence.
func TestAFailedWakeUpIsReportedWhereItWasAskedAndInOps(t *testing.T) {
	brain := &fakeBrain{fail: errors.New("POST \"https://api.example/v1\": 400 Bad Request {\n    \"message\": \"tools aren't supported\",\n    \"type\": \"invalid_request_error\"\n  }")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: what's in the README?")

	n.human.expect("why, in #ops", fromContaining("ledger", "#ops", `"message": "tools aren't supported"`))
	n.human.expect("kai saying it couldn't answer", fromContaining("kai", "#engineering", "couldn't answer"))
}

func TestWhatTheAgentSaidIsPostedEvenIfItsWorkEndsInAnError(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("PR #3 is up for review"), fail: errors.New("kai's desk used its budget and stopped")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: open a PR")

	n.human.expect("what kai said before stopping", fromContaining("kai", "#engineering", "PR #3 is up"))
}
