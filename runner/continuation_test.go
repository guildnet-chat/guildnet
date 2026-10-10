// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// IRC clients split a long message into several lines sent straight after
// each other. Those lines are one message: the agent gets all of it, and
// only the first line wakes anyone.

func continuing(within time.Duration) option {
	return func(cfg *config.Runner, _ *[]config.Persona) { cfg.ContinueWithin = within }
}

func TestALongMessageSplitIntoSeveralLinesReachesTheAgentWhole(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it")}
	n := startNetwork(t, brain, continuing(300*time.Millisecond))

	n.human.say("#engineering", "kai: rebase your change onto main, run the tests, and send it as a patch. Then check the PR's status")
	n.human.say("#engineering", "and only say it's ready when the checks pass.")

	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "on it"))
	req := brain.lastRequest(t)
	if !strings.Contains(req.Trigger.Text, "check the PR's status and only say it's ready when the checks pass.") {
		t.Errorf("the trigger should be the whole message, got %q", req.Trigger.Text)
	}
}

func TestTheRestOfASplitMessageDoesntWakeAnyoneOnItsOwn(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it")}
	n := startNetwork(t, brain, continuing(300*time.Millisecond))

	n.human.say("#engineering", "kai: please look at the build, it broke after")
	n.human.say("#engineering", "vera merged the approvals change")

	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "on it"))
	n.human.expectQuiet("vera waking for a mere mention in the second line", 600*time.Millisecond, from("vera", "#engineering"))
	brain.mu.Lock()
	defer brain.mu.Unlock()
	if len(brain.requests) != 1 {
		t.Errorf("%d wake-ups, want one for the whole message", len(brain.requests))
	}
}

func TestALineAddressedToSomeoneIsANewMessageEvenRightAfterAnother(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string { return "hi from " + req.Persona.Nick }}
	n := startNetwork(t, brain, continuing(300*time.Millisecond))

	n.human.say("#engineering", "kai: status?")
	n.human.say("#engineering", "vera: and yours?")

	n.human.expect("kai answering", fromContaining("kai", "#engineering", "hi from kai"))
	n.human.expect("vera answering too", fromContaining("vera", "#engineering", "hi from vera"))
}

func TestASplitDirectMessageReachesTheAgentWhole(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ok")}
	n := startNetwork(t, brain, continuing(300*time.Millisecond))

	n.human.say("kai", "can you check the ledger tests and tell me")
	n.human.say("kai", "which ones are slow?")

	n.human.expect("kai's reply", fromContaining("kai", "method", "ok"))
	brain.mu.Lock()
	defer brain.mu.Unlock()
	if len(brain.requests) != 1 || !strings.Contains(brain.requests[0].Trigger.Text, "tell me which ones are slow?") {
		t.Errorf("requests = %d, trigger %q", len(brain.requests), brain.requests[len(brain.requests)-1].Trigger.Text)
	}
}

func TestALaterLineIsntMergedEvenIfTheAgentWasBusyUntilThen(t *testing.T) {
	brain := &fakeBrain{reply: func(req agent.Request) string {
		if strings.Contains(req.Trigger.Text, "slow one") {
			time.Sleep(1500 * time.Millisecond) // kai is busy with something else
		}
		return "ok"
	}}
	n := startNetwork(t, brain, continuing(100*time.Millisecond))
	n.human.say("kai", "the slow one")
	n.human.expect("kai busy with it", awayFrom("kai"))

	n.human.say("#engineering", "kai: first question")
	time.Sleep(300 * time.Millisecond)
	n.human.say("#engineering", "unrelated: lunch is here")

	n.human.expect("kai's channel reply", fromContaining("kai", "#engineering", "ok"))
	brain.mu.Lock()
	defer brain.mu.Unlock()
	for _, req := range brain.requests {
		if req.Where == "#engineering" && req.Trigger.Text != "kai: first question" {
			t.Errorf("trigger = %q, want only the first question: the later line came well after it", req.Trigger.Text)
		}
	}
}
