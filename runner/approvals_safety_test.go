// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Someone taken out of the org keeps no say over approvals they were asked
// to decide while in it.
func TestSomeoneTakenOutCantDecideApprovals(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
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
	dana.say("GuildServ", "APPROVE 1")

	dana.expect("a refusal", guildServSaying("dana", "only"))
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
