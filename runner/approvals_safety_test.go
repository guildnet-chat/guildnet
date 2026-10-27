// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Someone taken out of the org keeps no say over approvals they were asked
// to decide while in it: not through the console, which still knows them by
// their account, and not over IRC, where their account is suspended (Ergo
// disconnects it), so nothing they could say would reach GuildServ.
func TestSomeoneTakenOutCantDecideApprovals(t *testing.T) {
	n := startNetwork(t, wantsToMerge(), withAPI())
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineering manager" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "SET kai REPORTS-TO dana")
	n.human.expect("kai now reports to dana", guildServSaying("method", "reports-to is now dana"))
	dana := n.joinAs("dana", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
	n.human.say("#engineering", "kai: ship it")
	dana.expect("dana asked", askedToApprove("dana"))

	n.human.say("GuildServ", "SET kai REPORTS-TO vera")
	n.human.expect("kai back to vera", guildServSaying("method", "reports-to is now vera"))
	n.human.say("GuildServ", "MEMBER REMOVE dana")
	n.human.expect("dana removed", guildServSaying("method", "dana"))

	if status, out := n.api("dana").call("POST", "/v1/approvals/1/approve", nil); status < 400 {
		t.Errorf("dana, taken out, approved through the console: %d %v", status, out)
	}
	var said []string
	for range 100 {
		if said = suspensions(n.srv.NickServ()); slices.Contains(said, "ADD dana") {
			break
		}
		time.Sleep(waitTimeout / 100)
	}
	if !slices.Contains(said, "ADD dana") {
		t.Errorf("dana's IRC account wasn't suspended: NickServ was told %v", n.srv.NickServ())
	}
	n.human.expectQuiet("kai carrying on", 300*time.Millisecond, fromContaining("kai", "#engineering", "merged"))
}

// Being asked isn't a say for good: someone who no longer may approve for
// the agent (here, it stopped reporting to them) can't decide what they
// were asked before, though they're still in the org.
func TestSomeoneWhoNoLongerMayApproveCantDecideWhatTheyWereAsked(t *testing.T) {
	n := startNetwork(t, wantsToMerge(), withAPI())
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineering manager" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	n.human.say("GuildServ", "SET kai REPORTS-TO dana")
	n.human.expect("kai now reports to dana", guildServSaying("method", "reports-to is now dana"))
	dana := n.joinAs("dana", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
	n.human.say("#engineering", "kai: ship it")
	dana.expect("dana asked", askedToApprove("dana"))

	n.human.say("GuildServ", "SET kai REPORTS-TO vera")
	n.human.expect("kai back to vera", guildServSaying("method", "reports-to is now vera"))

	if status, out := n.api("dana").call("POST", "/v1/approvals/1/approve", nil); status < 400 {
		t.Errorf("dana, no longer kai's manager, approved through the console: %d %v", status, out)
	}
	dana.say("GuildServ", "APPROVE 1")
	n.human.expectQuiet("kai carrying on", 300*time.Millisecond, fromContaining("kai", "#engineering", "merged"))
}

// However long what an agent wants to do is (a pull request's title can be
// long, and someone else's words), GuildServ's question keeps its own
// closing words, for chat's buttons to go by.
func TestALongApprovalStillEndsWithHowToAnswerIt(t *testing.T) {
	brain := &fakeBrain{
		reply: func(req agent.Request) string { return "" },
		approval: func(req agent.Request) *agent.ApprovalRequest {
			if req.Persona.Nick != "kai" || !strings.Contains(req.Trigger.Text, "ship it") {
				return nil
			}
			return &agent.ApprovalRequest{Tool: "github.merge_pull_request",
				Summary: "merge PR #17 '" + strings.Repeat("very long title ", 60) + "' into main", Resume: "kai-desk/merge-17"}
		},
	}
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")

	m := n.human.expect("an approval request", askedToApprove("method"))
	if !regexp.MustCompile(`^#1: .* Reply APPROVE 1, or DENY 1 <reason>\. Expires [^.]*\.$`).MatchString(m.text) {
		t.Errorf("the request should end with how to answer it: %q", m.text)
	}
}
