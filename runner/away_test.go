// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Agents show they're busy with IRC's own away status while they work.

func awayFrom(nick string) func(heard) bool {
	return func(m heard) bool { return m.away && m.text != "" && strings.EqualFold(m.from, nick) }
}

func backFrom(nick string) func(heard) bool {
	return func(m heard) bool { return m.away && m.text == "" && strings.EqualFold(m.from, nick) }
}

// slowBrain holds each reply until the test lets it go.
func slowBrain() (*fakeBrain, chan struct{}) {
	release := make(chan struct{})
	return &fakeBrain{reply: func(agent.Request) string {
		<-release
		return "done"
	}}, release
}

func TestAnAgentIsAwayWhileItWorksOnAReply(t *testing.T) {
	brain, release := slowBrain()
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: can you look at the build?")

	m := n.human.expect("kai going away", awayFrom("kai"))
	if !strings.Contains(m.text, "method") || !strings.Contains(m.text, "#engineering") {
		t.Errorf("kai's away message %q should say who it's replying to and where", m.text)
	}
	n.human.expectQuiet("kai coming back before replying", 200*time.Millisecond, backFrom("kai"))

	close(release)
	n.human.expect("kai's reply", from("kai", "#engineering"))
	n.human.expect("kai back from away", backFrom("kai"))
}

func TestAwayMessagesDontRevealWhoIsDMingAnAgent(t *testing.T) {
	brain, release := slowBrain()
	n := startNetwork(t, brain)

	n.human.say("kai", "private question")

	m := n.human.expect("kai going away", awayFrom("kai"))
	if strings.Contains(m.text, "method") {
		t.Errorf("kai's away message %q shouldn't name who DM'd it", m.text)
	}
	close(release)
	n.human.expect("kai back from away", backFrom("kai"))
}

func TestQuickRepliesDontFlickerAway(t *testing.T) {
	n := startNetwork(t, &fakeBrain{reply: replyWith("instant")})

	n.human.say("#engineering", "kai: quick one")
	n.human.expect("kai's reply", from("kai", "#engineering"))

	n.human.expectQuiet("an away change for a quick reply", 300*time.Millisecond, func(m heard) bool {
		return m.away && strings.EqualFold(m.from, "kai")
	})
}
