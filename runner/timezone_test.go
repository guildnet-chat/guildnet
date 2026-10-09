// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
)

// Times people and agents see are in the company's timezone.

func withTimezone(name string) option {
	return func(c *config.Runner, _ *[]config.Persona) {
		loc, err := time.LoadLocation(name)
		if err != nil {
			panic(err)
		}
		c.Location = loc
	}
}

func TestCatchupsShowTimesInTheCompanyTimezone(t *testing.T) {
	// Tokyo has no daylight saving, so its abbreviation is always JST.
	n := startNetwork(t, &fakeBrain{digest: "summary"}, withTimezone("Asia/Tokyo"))
	n.human.say("#engineering", "something to catch up on")
	time.Sleep(100 * time.Millisecond)

	n.human.say("GuildServ", "CATCHUP 1h")

	m := n.human.expect("the catch-up heading", func(m heard) bool {
		return m.from == "GuildServ" && strings.HasPrefix(m.text, "Catch-up since")
	})
	if !strings.Contains(m.text, "JST") {
		t.Errorf("heading %q should show the time in the company timezone (JST)", m.text)
	}
}

func TestAgentsSeeTimesInTheCompanyTimezone(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("ok")}
	n := startNetwork(t, brain, withTimezone("Asia/Tokyo"))

	n.human.say("#engineering", "kai: what time is it for you?")
	n.human.expect("kai's reply", from("kai", "#engineering"))

	if loc := brain.lastRequest(t).Location; loc == nil || loc.String() != "Asia/Tokyo" {
		t.Errorf("kai was given times in %v, want Asia/Tokyo", loc)
	}
}
