// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Approvals: a risky action pauses the agent until the first human up its
// reporting line decides. Kai reports to vera (an agent), who reports to
// method (the owner), so method is asked.

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func withClock(c *fakeClock) option {
	return func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.Clock = c.Now
		cfg.ApprovalCheckEvery = 20 * time.Millisecond
	}
}

// wantsToMerge makes kai pause for approval on its first wake-up, then
// reply with what happened once resumed.
func wantsToMerge() *fakeBrain {
	return &fakeBrain{
		reply: func(req agent.Request) string { return "" },
		approval: func(req agent.Request) *agent.ApprovalRequest {
			if req.Persona.Nick != "kai" || !strings.Contains(req.Trigger.Text, "ship it") {
				return nil
			}
			return &agent.ApprovalRequest{Tool: "github.merge_pull_request",
				Summary: "merge PR #17 'Retry webhooks' into main", Resume: "kai-desk/merge-17"}
		},
		afterResume: func(allow bool, reason string) string {
			if allow {
				return "merged PR #17"
			}
			return "ok, not merging: " + reason
		},
	}
}

func askedToApprove(to string) func(heard) bool {
	return guildServSaying(to, "APPROVE 1")
}

func TestARiskyActionAsksTheFirstHumanUpTheReportingLine(t *testing.T) {
	n := startNetwork(t, wantsToMerge())

	n.human.say("#engineering", "kai: ship it")

	m := n.human.expect("an approval request", askedToApprove("method"))
	for _, want := range []string{"#1", "kai", "merge PR #17 'Retry webhooks' into main", "DENY 1"} {
		if !strings.Contains(m.text, want) {
			t.Errorf("approval request %q should mention %q", m.text, want)
		}
	}
	n.human.expect("a notice in the channel", fromContaining("ledger", "#engineering", "approval #1"))
}

// Firing an agent drops what it was waiting on: a new agent hired with the
// same nick doesn't inherit it, and approving it runs nothing.
func TestFiringAnAgentDeniesWhatItWasWaitingOn(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	n.human.say("GuildServ", "FIRE kai")
	n.human.expect("kai fired", guildServSaying("method", "fired kai"))
	n.human.say("GuildServ", `HIRE kai ROLE "Engineer" TEAM Engineering`)
	n.human.expect("a new kai hired", guildServSaying("method", "hired kai"))
	n.human.say("GuildServ", "APPROVE 1")

	n.human.expect("the old approval refused", guildServSaying("method", "already denied"))
	brain.mu.Lock()
	defer brain.mu.Unlock()
	if len(brain.resumes) != 0 {
		t.Errorf("the fired agent's approval carried on: %+v", brain.resumes)
	}
}

// What runs is what's approved, however long: the call's whole input is kept
// with the approval, for the console to show.
func TestAnApprovalKeepsTheCallsWholeInput(t *testing.T) {
	long := strings.Repeat("a line of the page. ", 1000)
	brain := wantsToMerge()
	brain.approval = func(req agent.Request) *agent.ApprovalRequest {
		if req.Persona.Nick != "kai" || !strings.Contains(req.Trigger.Text, "ship it") {
			return nil
		}
		return &agent.ApprovalRequest{Tool: "notion_create_page", Summary: "use Notion: create_page", Resume: "kai-desk/page",
			Input: map[string]any{"title": "Launch", "body": long}}
	}
	n := startNetwork(t, brain, withAPI())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	_, out := n.api("method").call("GET", "/v1/approvals", nil)
	list, _ := out["approvals"].([]any)
	if len(list) != 1 {
		t.Fatalf("approvals: %v", out)
	}
	in, _ := list[0].(map[string]any)["input"].(map[string]any)
	if in["body"] != long || in["title"] != "Launch" {
		t.Errorf("the approval's input = %.200v, want the whole call", in)
	}
}

func TestTheApproverIsTheAgentsHumanManagerNotAlwaysTheOwner(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineering manager" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "SET kai REPORTS-TO dana")
	n.human.expect("kai now reports to dana", guildServSaying("method", "reports-to is now dana"))
	dana := n.joinAs("dana", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")

	n.human.say("#engineering", "kai: ship it")

	dana.expect("dana asked to approve", askedToApprove("dana"))
	n.human.expectQuiet("the owner asked instead", 300*time.Millisecond, askedToApprove("method"))

	// The owner can still step in.
	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
}

func TestApprovingLetsTheAgentCarryOn(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	n.human.say("GuildServ", "APPROVE 1")

	n.human.expect("a confirmation", guildServSaying("method", "approved #1"))
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
	if r := brain.lastResume(t); !r.allow || r.token != "kai-desk/merge-17" {
		t.Errorf("kai resumed with %+v, want allow on its merge", r)
	}
	n.human.expect("a record in #ops", fromContaining("ledger", "#ops", "approved #1"))
}

func TestDenyingSendsTheReasonBack(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	n.human.say("GuildServ", "DENY 1 not before the launch")

	n.human.expect("kai adjusting", fromContaining("kai", "#engineering", "not merging: not before the launch"))
}

func TestOnlyTheReportingLineCanDecide(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Head of Sales" TEAM Marketing`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	dana := n.joinAs("dana", "#general")
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	dana.say("GuildServ", "APPROVE 1")

	dana.expect("a refusal", guildServSaying("dana", "only"))
	n.human.expectQuiet("kai carrying on", 300*time.Millisecond, from("kai", "#engineering"))
}

func TestAnApprovalIsOnlyDecidedOnce(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("approved", guildServSaying("method", "approved #1"))

	n.human.say("GuildServ", "DENY 1 changed my mind")

	n.human.expect("already decided", guildServSaying("method", "already"))
}

func TestWhileWaitingTheAgentSaysSoAndCatchesUpAfter(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	brain.mu.Lock()
	brain.reply = replyWith("on it")
	brain.mu.Unlock()

	n.human.say("#engineering", "kai: also, update the changelog")
	n.human.expect("a waiting notice", fromContaining("ledger", "#engineering", "kai is waiting on approval #1"))
	n.human.expectQuiet("kai replying while it waits", 300*time.Millisecond, fromContaining("kai", "#engineering", "on it"))

	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
	n.human.expect("kai picking up the parked request", fromContaining("kai", "#engineering", "on it"))
}

func TestUnansweredApprovalsExpireAfter24WorkingHours(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)} // a Wednesday
	brain := wantsToMerge()
	n := startNetwork(t, brain, withClock(clock))
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	clock.Advance(23 * time.Hour)
	n.human.expectQuiet("an early expiry", 200*time.Millisecond, fromContaining("kai", "#engineering", "not merging"))

	clock.Advance(2 * time.Hour)
	n.human.expect("kai told it expired", fromContaining("kai", "#engineering", "not merging"))
	if r := brain.lastResume(t); r.allow || !strings.Contains(r.reason, "no decision") {
		t.Errorf("an expired approval should resume as a denial saying so, got %+v", r)
	}
}

func TestApprovalsDontExpireOverTheWeekend(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 17, 0, 0, 0, time.UTC)} // Friday 5pm
	n := startNetwork(t, wantsToMerge(), withClock(clock))
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	clock.Advance(64 * time.Hour) // Monday 9am
	n.human.expectQuiet("an expiry over the weekend", 200*time.Millisecond, fromContaining("kai", "#engineering", "not merging"))

	clock.Advance(9 * time.Hour) // Monday 6pm
	n.human.expect("kai told it expired", fromContaining("kai", "#engineering", "not merging"))
}

func TestCatchupListsApprovalsWaitingOnYouFirst(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	n.human.say("GuildServ", "CATCHUP 1h")

	m := n.human.expect("the first line of the catch-up", func(m heard) bool {
		return m.from == "GuildServ" && strings.Contains(m.text, "Waiting on you")
	})
	if !strings.Contains(m.text, "#1") || !strings.Contains(m.text, "merge PR #17") {
		t.Errorf("catch-up line %q should list approval #1", m.text)
	}
}

func TestApprovalsSurviveARestart(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	n.human.say("GuildServ", "APPROVE 1")

	n.human.expect("kai carrying on after the restart", fromContaining("kai", "#engineering", "merged PR #17"))
}

// Approvals move on when nobody answers. Who's asked, in order, is settled
// when the approval is raised: the humans up the agent's reporting line who
// may approve, then the leads of its team (and the teams it's inside), then
// the owner. Each gets a while (4 working hours unless the owner says
// otherwise) before it moves to the next; anyone on the way can decide.

// kaiUnderDana makes kai report to dana, a human in Engineering, and sam a
// co-lead of Engineering, both logged in.
func kaiUnderDana(t *testing.T, n *network) (dana, sam *human) {
	t.Helper()
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineering manager" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Engineer" TEAM Engineering`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	n.human.say("GuildServ", "PROMOTE sam")
	n.human.expect("sam a lead", guildServSaying("method", "sam is now lead"))
	n.human.say("GuildServ", "SET kai REPORTS-TO dana")
	n.human.expect("kai now reports to dana", guildServSaying("method", "reports-to is now dana"))
	return n.joinAs("dana", "#general"), n.joinAs("sam", "#general")
}

func TestAnUnansweredApprovalMovesOnToTheNextPerson(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)} // a Wednesday
	n := startNetwork(t, wantsToMerge(), withClock(clock))
	dana, sam := kaiUnderDana(t, n)
	n.human.say("#engineering", "kai: ship it")
	dana.expect("dana asked first", askedToApprove("dana"))

	clock.Advance(3 * time.Hour)
	sam.expectQuiet("moving on too soon", 200*time.Millisecond, askedToApprove("sam"))
	clock.Advance(time.Hour + time.Minute)
	m := sam.expect("sam asked next, as a lead of kai's team", askedToApprove("sam"))
	if !strings.Contains(m.text, "dana") {
		t.Errorf("sam's request %q should say dana didn't answer", m.text)
	}
	clock.Advance(4*time.Hour + time.Minute)
	n.human.expect("the owner asked last", askedToApprove("method"))

	// Anyone on the way can still decide.
	dana.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
}

func TestTheOwnerSetsHowLongApprovalsWaitBeforeMovingOn(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
	n := startNetwork(t, wantsToMerge(), withClock(clock))
	dana, sam := kaiUnderDana(t, n)

	dana.say("GuildServ", "APPROVALS AFTER 30m")
	dana.expect("a refusal", guildServSaying("dana", "only"))
	n.human.say("GuildServ", "APPROVALS AFTER 1h")
	n.human.expect("the owner's setting", guildServSaying("method", "1h"))

	n.human.say("#engineering", "kai: ship it")
	dana.expect("dana asked", askedToApprove("dana"))
	clock.Advance(61 * time.Minute)
	sam.expect("moved on after an hour", askedToApprove("sam"))
}

func TestApprovalsCanBeLeftWithTheFirstPerson(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
	n := startNetwork(t, wantsToMerge(), withClock(clock))
	dana, sam := kaiUnderDana(t, n)
	n.human.say("GuildServ", "APPROVALS AFTER off")
	n.human.expect("the owner's setting", guildServSaying("method", "stay"))

	n.human.say("#engineering", "kai: ship it")
	dana.expect("dana asked", askedToApprove("dana"))
	clock.Advance(20 * time.Hour)
	sam.expectQuiet("moving on", 300*time.Millisecond, askedToApprove("sam"))
}
