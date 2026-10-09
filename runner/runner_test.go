// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/ledger"
)

func TestAgentRepliesInChannelWhenMentioned(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it, fixing the flaky test now")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: can you look at the flaky test?")

	n.human.expect("kai's reply in #engineering", fromContaining("kai", "#engineering", "on it"))
	n.human.expectQuiet("a reply from vera", 300*time.Millisecond, from("vera", "#engineering"))
}

func TestAgentSeesTopicAndRecentConversationWhenWoken(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ack")}
	n := startNetwork(t, brain)

	n.human.setTopic("#engineering", "Ship M0 by Friday")
	n.human.say("#engineering", "the build went red after the last merge")
	n.human.say("#engineering", "kai: any idea why?")
	n.human.expect("kai's reply", from("kai", "#engineering"))

	req := brain.lastRequest(t)
	if req.Persona.Nick != "kai" || req.Where != "#engineering" {
		t.Fatalf("woke %s in %s, want kai in #engineering", req.Persona.Nick, req.Where)
	}
	if req.Topic != "Ship M0 by Friday" {
		t.Errorf("topic = %q, want the channel topic", req.Topic)
	}
	var transcript []string
	for _, l := range req.Transcript {
		transcript = append(transcript, l.Text)
	}
	joined := strings.Join(transcript, "\n")
	if !strings.Contains(joined, "build went red") || !strings.HasSuffix(joined, "kai: any idea why?") {
		t.Errorf("transcript should include earlier messages and end with the trigger, got:\n%s", joined)
	}
}

func TestOnlyTheAddressedAgentWakesWhenOthersAreMerelyMentioned(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string { return "reply from " + req.Persona.Nick }}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "vera: what's the plan today? pull kai in on the runner side")

	n.human.expect("vera's reply", fromContaining("vera", "#engineering", "reply from vera"))
	n.human.expectQuiet("kai jumping in", 500*time.Millisecond, from("kai", "#engineering"))
}

func TestAllNamedAgentsWakeWhenNoOneIsAddressedFirst(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string { return "reply from " + req.Persona.Nick }}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "morning vera and kai, quick sync?")

	n.human.expect("vera's reply", fromContaining("vera", "#engineering", "reply from vera"))
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "reply from kai"))
}

func TestWokenAgentSeesMessagesPostedAfterTheOneThatWokeIt(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	brain := &fakeBrain{reply: func(req agent.Request) string {
		if req.Persona.Nick == "kai" {
			once.Do(func() { <-release }) // kai is busy in a DM while the channel moves on
		}
		return "ok"
	}}
	n := startNetwork(t, brain)

	n.human.say("kai", "got a sec?")
	time.Sleep(200 * time.Millisecond) // kai is now busy with the DM
	n.human.say("#engineering", "kai: what's blocking the runner?")
	n.human.say("#engineering", "vera: the plan is to ship M0 today")
	n.human.expect("vera's reply", from("vera", "#engineering"))
	close(release)
	n.human.expect("kai's DM reply", from("kai", "method"))
	n.human.expect("kai's channel reply", from("kai", "#engineering"))

	// By the time kai gets to the channel question, the plan and vera's
	// reply have been posted after it; kai should see them.
	var got agent.Request
	brain.mu.Lock()
	for _, r := range brain.requests {
		if r.Persona.Nick == "kai" && r.Where == "#engineering" {
			got = r
		}
	}
	brain.mu.Unlock()
	var texts []string
	for _, l := range got.Transcript {
		texts = append(texts, l.Nick+": "+l.Text)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "ship M0 today") || !strings.Contains(joined, "vera: ok") {
		t.Errorf("kai's view of the conversation stopped at the message that woke it:\n%s", joined)
	}
}

func TestAgentRepliesByDMWhenDMd(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("sure, happy to pair on it")}
	n := startNetwork(t, brain)

	n.human.say("kai", "got a minute to pair?")

	n.human.expect("kai's DM back", fromContaining("kai", "method", "happy to pair"))
	if req := brain.lastRequest(t); !strings.Contains(req.Where, "DM") {
		t.Errorf("agent was told it's in %q, want a DM", req.Where)
	}
}

func TestAgentsStayQuietWhenNotAddressed(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("I have thoughts")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "just thinking out loud about the roadmap")

	n.human.expectQuiet("an agent reply", 500*time.Millisecond, fromAgent("#engineering"))
}

func TestUnaddressedMessageGoesToTheAgentTheRelevanceFilterPicks(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("that's the webhook retry, I'll fix it"), pick: []string{"kai"}}
	n := startNetwork(t, brain, withRelevance())

	n.human.say("#engineering", "anyone know why stripe webhooks are failing?")

	n.human.expect("kai picking it up", fromContaining("kai", "#engineering", "webhook retry"))
	n.human.expectQuiet("a reply from vera", 300*time.Millisecond, from("vera", "#engineering"))
}

func TestPassReplyPostsNothing(t *testing.T) {
	brain := &fakeBrain{reply: replyWith(agent.Pass)}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: fyi, deploy is done")

	n.human.expectQuiet("a reply from kai", 500*time.Millisecond, from("kai", "#engineering"))
}

func TestAgentDoesNotAddressItself(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("kai: wiring up the ledger bot")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: what are you on?")

	m := n.human.expect("kai's reply", from("kai", "#engineering"))
	if m.text != "wiring up the ledger bot" {
		t.Errorf("kai said %q, want it without its own nick in front", m.text)
	}
}

func TestMeLinesArriveAsActionsAndLongRepliesAreSplit(t *testing.T) {
	long := strings.Repeat("lorem ipsum ", 80) // ~960 bytes, more than one IRC line
	brain := &fakeBrain{reply: replyWith("/me starts on the landing page\n" + long)}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: go")

	action := n.human.expect("an action", from("kai", "#engineering"))
	if !action.action || action.text != "starts on the landing page" {
		t.Errorf("first line = %+v, want the /me action", action)
	}
	var got []string
	for len(strings.Join(got, " ")) < len(strings.TrimSpace(long)) {
		m := n.human.expect("the rest of the long reply", from("kai", "#engineering"))
		if len(m.text) > 400 {
			t.Fatalf("a line was %d bytes; IRC lines must stay short", len(m.text))
		}
		got = append(got, strings.TrimSpace(m.text))
	}
	if strings.Join(got, " ") != strings.TrimSpace(long) {
		t.Errorf("long reply came through altered")
	}
}

func TestAgentsStopReplyingToEachOtherAfterTheLoopLimit(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string {
		if req.Persona.Nick == "vera" {
			return "kai: over to you"
		}
		return "vera: back to you"
	}}
	n := startNetwork(t, brain, withLoopLimit(3))

	n.human.say("#engineering", "vera: kick it off")

	n.human.expect("vera starting", fromContaining("vera", "#engineering", "over to you"))
	n.human.expect("kai replying", fromContaining("kai", "#engineering", "back to you"))
	n.human.expect("vera replying", fromContaining("vera", "#engineering", "over to you"))
	n.human.expect("the loop guard notice", fromContaining("ledger", "#engineering", "loop guard"))
	n.human.expectQuiet("agent chatter past the loop limit of 3", 500*time.Millisecond, fromAgent("#engineering"))

	// A human speaking lifts the guard.
	n.human.say("#engineering", "kai: one more thing")
	n.human.expect("kai answering the human", from("kai", "#engineering"))
}

func TestTheLoopLimitCountsRepliesNotLines(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string {
		if req.Persona.Nick == "vera" {
			return "good point\nkai: over to you"
		}
		return "vera: back to you"
	}}
	n := startNetwork(t, brain, withLoopLimit(3))

	n.human.say("#engineering", "vera: kick it off")

	// Three replies, even though vera's are two lines each.
	n.human.expect("vera's first reply", fromContaining("vera", "#engineering", "over to you"))
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "back to you"))
	n.human.expect("vera's second reply", fromContaining("vera", "#engineering", "over to you"))
	n.human.expect("the loop guard notice", fromContaining("ledger", "#engineering", "loop guard"))
	n.human.expect("vera's first line", fromContaining("vera", "#engineering", "good point"))
	n.human.expect("vera's first line again", fromContaining("vera", "#engineering", "good point"))
	n.human.expectQuiet("a fourth reply", 500*time.Millisecond, fromAgent("#engineering"))
}

func TestAgentsStopWhenTheDailyBudgetIsUsedUp(t *testing.T) {
	// One call on Sonnet 5 at 1M output tokens costs $10, over the $5 budget.
	brain := &fakeBrain{reply: replyWith("done"), usage: ledger.Usage{Output: 1_000_000}}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: first task")
	n.human.expect("kai's reply", from("kai", "#engineering"))
	n.human.expect("the budget-used-up notice", fromContaining("ledger", "#ops", "used up"))

	n.human.say("#engineering", "kai: second task")
	n.human.expect("a notice that kai is off", fromContaining("ledger", "#ops", "kai is off"))
	n.human.expectQuiet("another reply from kai", 300*time.Millisecond, from("kai", "#engineering"))
}

// The budget is checked before each model call of a wake-up, not just when
// it starts: a long wake-up stops once the money's used up.
func TestAWakeUpOfManyCallsStopsAtTheBudget(t *testing.T) {
	// Each call is $3 on Sonnet 5 (300k output tokens), against the $5 budget.
	call := ledger.Usage{Output: 300_000}
	brain := &fakeBrain{reply: replyWith("done"), calls: []ledger.Usage{call, call, call, call}}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: a long task")
	n.human.expect("why kai stopped", fromContaining("ledger", "#ops", "used up"))
	n.human.say("#engineering", "!spend")

	m := n.human.expect("the spend report", fromContaining("ledger", "#engineering", "spend"))
	if !strings.Contains(m.text, "$6.000 of $5.00") {
		t.Errorf("spend report %q: want two $3 calls, the second crossing the budget, and no more", m.text)
	}
}

// Pausing stops agents carrying on after an approval too, until they're
// resumed.
func TestAnApprovalDecidedWhilePausedWaitsForResume(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	n.human.say("#engineering", "!pause")
	n.human.expect("paused", fromContaining("ledger", "#engineering", "paused"))

	n.human.say("GuildServ", "APPROVE 1")
	n.human.expectQuiet("kai carrying on while paused", 500*time.Millisecond, fromContaining("kai", "#engineering", "merged PR #17"))

	n.human.say("#engineering", "!resume")
	n.human.expect("kai carrying on once resumed", fromContaining("kai", "#engineering", "merged PR #17"))
}

func TestOneCallCrossingSeveralThresholdsPostsOneBudgetNotice(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("done"), usage: ledger.Usage{Output: 1_000_000}}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: big task")
	n.human.expect("the budget notice", fromContaining("ledger", "#ops", "budget"))
	n.human.expectQuiet("a second budget notice", 300*time.Millisecond, fromContaining("ledger", "#ops", "budget"))
}

func TestBudgetWarningsAppearInOps(t *testing.T) {
	// $2.60 of a $5 budget: crosses 50%.
	brain := &fakeBrain{reply: replyWith("done"), usage: ledger.Usage{Output: 260_000}}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: task")

	n.human.expect("a 50% warning", fromContaining("ledger", "#ops", "50%"))
}

func TestPauseStopsAgentsAndResumeStartsThem(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("here")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "!pause")
	n.human.expect("pause confirmation", fromContaining("ledger", "#engineering", "paused"))
	n.human.say("#engineering", "kai: you there?")
	n.human.expectQuiet("a reply while paused", 500*time.Millisecond, from("kai", "#engineering"))

	n.human.say("#engineering", "!resume")
	n.human.expect("resume confirmation", fromContaining("ledger", "#engineering", "resumed"))
	n.human.say("#engineering", "kai: now?")
	n.human.expect("kai's reply after resume", from("kai", "#engineering"))
}

func TestSpendCommandReportsTodaysSpend(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("done"), usage: ledger.Usage{Input: 1000, Output: 1000}}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: task")
	n.human.expect("kai's reply", from("kai", "#engineering"))
	n.human.say("#engineering", "!spend")

	m := n.human.expect("a spend report", fromContaining("ledger", "#engineering", "spend"))
	if !strings.Contains(m.text, "kai $0.012") {
		t.Errorf("spend report %q should show kai's $0.012 (1k in + 1k out on Sonnet 5)", m.text)
	}
}
