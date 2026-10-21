// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

// What an agent says reaches IRC as text: a model, or a web page it read,
// can't have it send CTCP requests, colours or bold, in its reply, its steps,
// or what its tools found.
func TestAnAgentsRepliesAndStepsCarryNoControlCodes(t *testing.T) {
	brain := working("runs \x01PING x\x01 go test", "=> opened \x02PR #3\x02")
	brain.reply = replyWith("tests \x034,12pass\x0f \x01VERSION\x01")
	n := startNetwork(t, brain)
	n.human.say("GuildServ", "TEAM SET Engineering CHATTER steps")
	n.human.expect("chatter set", guildServSaying("method", "chatter steps"))

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai's step, as text", actionFrom("kai", "#engineering", "runs PING x go test"))
	n.human.expect("what it achieved, as text", actionFrom("kai", "#engineering", "opened PR #3"))
	reply := n.human.expect("kai's reply", fromContaining("kai", "#engineering", "tests"))
	if reply.text != "tests pass VERSION" {
		t.Errorf("kai's reply = %q, want %q", reply.text, "tests pass VERSION")
	}
	for _, m := range n.human.all() {
		if m.from == "kai" && strings.ContainsFunc(m.text, unicode.IsControl) {
			t.Errorf("kai sent a control code: %q", m.text)
		}
	}
}

// One reply highlights at most five people: past that, names still read the
// same but ping nobody.
func TestAReplyCantHighlightACrowd(t *testing.T) {
	names := []string{"amy", "bea", "cal", "dan", "eve", "fay", "gus", "hal"}
	n := startNetwork(t, &fakeBrain{reply: replyWith("ping " + strings.Join(names, " ") + ", and amy again")})
	for _, name := range names {
		inOrg(t, n.human, name)
	}

	n.human.say("#engineering", "kai: who's around?")

	reply := n.human.expect("kai's reply", fromContaining("kai", "#engineering", "ping"))
	var pinged []string
	for _, w := range strings.FieldsFunc(reply.text, func(c rune) bool { return c == ' ' || c == ',' }) {
		if slices.Contains(names, w) && !slices.Contains(pinged, w) {
			pinged = append(pinged, w)
		}
	}
	if !slices.Equal(pinged, names[:5]) {
		t.Errorf("kai's reply %q pings %v, want the first five: %v", reply.text, pinged, names[:5])
	}
	if got := strings.ReplaceAll(reply.text, "​", ""); got != "ping "+strings.Join(names, " ")+", and amy again" {
		t.Errorf("kai's reply reads %q, want it to read as written", got)
	}
}
