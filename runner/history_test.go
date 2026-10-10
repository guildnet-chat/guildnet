// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"
)

// Agents keep their memory of recent conversation across runner restarts by
// reading channel history back from the IRC server.

func TestAgentsRememberTheConversationAfterARestart(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ok")}
	n := startNetwork(t, brain)
	n.human.say("#engineering", "heads up: the launch moved to Friday")
	n.human.say("#engineering", "and the demo is at 3pm")
	time.Sleep(200 * time.Millisecond) // let the server store them

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	n.human.say("#engineering", "kai: when's the launch?")
	n.human.expect("kai's reply", from("kai", "#engineering"))

	var said []string
	for _, l := range brain.lastRequest(t).Transcript {
		said = append(said, l.Text)
	}
	joined := strings.Join(said, "\n")
	if !strings.Contains(joined, "launch moved to Friday") || !strings.Contains(joined, "demo is at 3pm") {
		t.Errorf("after a restart kai should still see what was said before it; saw:\n%s", joined)
	}
}

func TestReplayedHistoryLeavesOutJoinAndQuitNoise(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ok")}
	n := startNetwork(t, brain)
	n.human.say("#engineering", "the launch moved to Friday")
	time.Sleep(200 * time.Millisecond)

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	n.human.say("#engineering", "kai: when's the launch?")
	n.human.expect("kai's reply", from("kai", "#engineering"))

	for _, l := range brain.lastRequest(t).Transcript {
		if strings.Contains(l.Text, "joined the channel") || strings.EqualFold(l.Nick, "HistServ") {
			t.Errorf("kai was shown server noise: <%s> %s", l.Nick, l.Text)
		}
	}
}

func TestReplayedHistoryDoesNotWakeAgents(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("here")}
	n := startNetwork(t, brain)
	// An old message in a channel nobody from the runner is in yet.
	n.human.conn.Join("#support")
	n.waitForMembers("#support", "method")
	n.human.say("#support", "kai: can you look at ticket 12?")
	time.Sleep(200 * time.Millisecond)

	// Moving kai there makes the runner read #support's history while kai
	// is already running. That old message must not wake it.
	n.human.say("GuildServ", "SET kai CHANNELS #general,#engineering,#support")
	n.human.expect("a confirmation", guildServSaying("method", "kai"))
	n.waitForMembers("#support", "kai", "ledger", "method")

	n.human.expectQuiet("kai answering the old message", time.Second, from("kai", "#support"))
}
