// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"testing"
	"time"
)

// A restart (a deploy, a crash) changes nothing people decided: a paused
// network stays paused, and what was approved is carried on with, once.

func TestAPausedNetworkStaysPausedAfterARestart(t *testing.T) {
	n := startNetwork(t, &fakeBrain{reply: replyWith("here")})
	n.human.say("#engineering", "!pause")
	n.human.expect("paused", fromContaining("ledger", "#engineering", "paused"))

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	n.human.say("#engineering", "kai: you there?")

	n.human.expectQuiet("kai answering after the restart", 500*time.Millisecond, from("kai", "#engineering"))
	n.human.say("#engineering", "!resume")
	n.human.expect("resumed", fromContaining("ledger", "#engineering", "resumed"))
	n.human.say("#engineering", "kai: now?")
	n.human.expect("kai answering once resumed", from("kai", "#engineering"))
}

func TestWhatWasApprovedBeforeARestartIsCarriedOnWithAfterIt(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	// Approved while paused: kai is to carry on once resumed, and the
	// runner restarts before that.
	n.human.say("#engineering", "!pause")
	n.human.expect("paused", fromContaining("ledger", "#engineering", "paused"))
	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("approved", guildServSaying("method", "#1"))

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	n.human.say("#engineering", "!resume")

	n.human.expect("kai carrying on after the restart", fromContaining("kai", "#engineering", "merged PR #17"))
}

func TestWhatWasCarriedOnWithIsntDoneAgainAfterARestart(t *testing.T) {
	n := startNetwork(t, wantsToMerge())
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")

	n.human.expectQuiet("kai doing it again", time.Second, fromContaining("kai", "#engineering", "merged PR #17"))
}
