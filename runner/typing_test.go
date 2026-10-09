// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/ergotest"
)

// Agents show they're working the way people do in chat: typing while they
// work, and answering in reply to the message that woke them. Run against
// a real Ergo, which gives every message an id and passes typing notices
// on (IRCv3 +typing and +draft/reply).

func TestAgentsShowTheyAreTypingWhileTheyWork(t *testing.T) {
	brain, release := slowBrain()
	n := startRealNetwork(t, brain)

	n.human.say("#engineering", "kai: are the webhook retries fixed?")
	n.human.expect("kai typing", typingFrom("kai", "#engineering", "active"))
	n.human.expect("kai still typing a few seconds on", typingFrom("kai", "#engineering", "active"))
	close(release)
	n.human.expect("kai's answer", from("kai", "#engineering"))
	answered := len(n.human.all())
	time.Sleep(4 * time.Second)

	for _, m := range n.human.all()[answered:] {
		if typingFrom("kai", "#engineering", "active")(m) {
			t.Fatalf("kai kept typing after answering: %+v", m)
		}
	}
}

func TestAnAgentThatHasNothingToSayStopsTyping(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{}) // no reply: it passes

	n.human.say("#engineering", "kai: never mind, sorted it")

	n.human.expect("kai typing", typingFrom("kai", "#engineering", "active"))
	n.human.expect("kai done typing", typingFrom("kai", "#engineering", "done"))
}

func TestAgentsAnswerInReplyToWhatWokeThem(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{reply: replyWith("They're fixed: PR #42.")})
	watcher := n.watcher()

	n.human.say("#engineering", "kai: are the webhook retries fixed?")
	asked := watcher.expect("the question", from(ownerAccount, "#engineering"))
	answer := watcher.expect("kai's answer", from("kai", "#engineering"))

	if asked.msgid == "" {
		t.Fatal("Ergo should give every message an id")
	}
	if answer.replyTo != asked.msgid {
		t.Errorf("kai's answer replies to %q, want the question %q", answer.replyTo, asked.msgid)
	}
}

func TestALongAnswerIsAReplyToo(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{reply: replyWith("They're fixed.\nThe timer raced with Stop().\nPR #42 has the fix.")})
	watcher := n.watcher()

	n.human.say("#engineering", "kai: are the webhook retries fixed?")
	asked := watcher.expect("the question", from(ownerAccount, "#engineering"))
	answer := watcher.expect("kai's answer", from("kai", "#engineering"))

	if answer.text != "They're fixed.\nThe timer raced with Stop().\nPR #42 has the fix." {
		t.Fatalf("the answer should arrive as one message: %q", answer.text)
	}
	if answer.replyTo == "" || answer.replyTo != asked.msgid {
		t.Errorf("kai's multiline answer replies to %q, want the question %q", answer.replyTo, asked.msgid)
	}
}

// watcher is someone else in #engineering, seeing what any IRC client sees,
// multiline included.
func (n *realNetwork) watcher() *human {
	n.t.Helper()
	if err := n.ergo.Register("sam", testPassword); err != nil {
		n.t.Fatal(err)
	}
	w := joinHumanWith(n.t, n.ergo.Addr, "sam", "sam", testPassword, true, "#engineering")
	n.waitFor("#engineering", "sam")
	return w
}

func TestWithoutMultilineTheAnswerIsStillAReply(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{reply: replyWith("They're fixed.\nPR #42 has the fix.")}, ergotest.WithoutMultiline())
	watcher := n.watcher()

	n.human.say("#engineering", "kai: are the webhook retries fixed?")
	asked := watcher.expect("the question", from(ownerAccount, "#engineering"))
	first := watcher.expect("kai's first line", fromContaining("kai", "#engineering", "fixed."))
	second := watcher.expect("kai's second line", fromContaining("kai", "#engineering", "PR #42"))

	if first.replyTo != asked.msgid || asked.msgid == "" {
		t.Errorf("kai's first line replies to %q, want the question %q", first.replyTo, asked.msgid)
	}
	if second.replyTo != "" {
		t.Errorf("only the first line is the reply; the second says it replies to %q", second.replyTo)
	}
}
