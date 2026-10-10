// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/models"
)

// Catch-up: people who were away get a short summary of what they missed.

func withCatchupAfter(d time.Duration) option {
	return func(c *config.Runner, _ *[]config.Persona) { c.Catchup.After = d }
}

func (b *fakeBrain) lastCatchup(t *testing.T) agent.CatchupRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.catchups) == 0 {
		t.Fatal("no catch-up was written")
	}
	return b.catchups[len(b.catchups)-1]
}

func (b *fakeBrain) catchupCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.catchups)
}

// said flattens a catch-up request into "#chan <nick> text" lines.
func said(req agent.CatchupRequest) string {
	var out []string
	for _, ch := range req.Channels {
		for _, l := range ch.Lines {
			out = append(out, ch.Name+" <"+l.Nick+"> "+l.Text)
		}
	}
	return strings.Join(out, "\n")
}

func TestComingBackAfterBeingAwayGetsACatchup(t *testing.T) {
	brain := &fakeBrain{digest: "For you: dana asked about the demo.\n#engineering: shipping Friday."}
	n := startNetwork(t, brain, withCatchupAfter(200*time.Millisecond))
	dana := joinHuman(t, n.srv.Addr(), "dana", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
	dana.say("#engineering", "old news from before")
	time.Sleep(100 * time.Millisecond)

	n.human.away("Auto-away")
	time.Sleep(100 * time.Millisecond)
	dana.say("#engineering", "we decided to ship Friday")
	dana.say("#engineering", "method: can you run the demo?")
	time.Sleep(300 * time.Millisecond)
	n.human.back()

	n.human.expect("the catch-up", func(m heard) bool {
		return m.from == "GuildServ" && m.to == "method" && strings.Contains(m.text, "shipping Friday")
	})
	req := brain.lastCatchup(t)
	if req.For != "method" {
		t.Errorf("catch-up written for %q, want method", req.For)
	}
	got := said(req)
	if !strings.Contains(got, "ship Friday") || !strings.Contains(got, "run the demo") {
		t.Errorf("catch-up should cover what was said while away:\n%s", got)
	}
	if strings.Contains(got, "old news") {
		t.Errorf("catch-up should leave out what was said before leaving:\n%s", got)
	}
}

func TestShortAbsenceGetsNoCatchup(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain, withCatchupAfter(time.Hour))

	n.human.away("brb")
	n.human.say("#engineering", "a message while away")
	n.human.back()

	n.human.expectQuiet("a catch-up", 500*time.Millisecond, fromGuildServ("method"))
	if c := brain.catchupCount(); c != 0 {
		t.Errorf("wrote %d catch-ups for a short absence, want none", c)
	}
}

func TestCatchupCommandSummarizesRecentConversation(t *testing.T) {
	brain := &fakeBrain{digest: "#general: people said hi."}
	n := startNetwork(t, brain)
	dana := joinHuman(t, n.srv.Addr(), "dana", "#general", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
	dana.say("#general", "hi all")
	dana.say("#engineering", "the build is green again")
	time.Sleep(100 * time.Millisecond)

	n.human.say("GuildServ", "CATCHUP")

	n.human.expect("the catch-up", func(m heard) bool {
		return m.from == "GuildServ" && strings.Contains(m.text, "people said hi")
	})
	got := said(brain.lastCatchup(t))
	if !strings.Contains(got, "#general <dana> hi all") || !strings.Contains(got, "#engineering <dana> the build is green again") {
		t.Errorf("CATCHUP should cover every channel you're in:\n%s", got)
	}
}

// A catch-up on a tier runs on that tier's model from the providers the
// network has when it's written.
func TestACatchupRunsOnItsTiersModelFromTodaysProviders(t *testing.T) {
	brain := &fakeBrain{digest: "#general: people said hi."}
	n := startNetwork(t, brain, withProviders(models.OpenAI), func(c *config.Runner, _ *[]config.Persona) { c.Catchup.Model = "fast" })
	dana := joinHuman(t, n.srv.Addr(), "dana", "#general")
	n.waitForMembers("#general", "dana", "kai", "ledger", "method", "vera")
	dana.say("#general", "hi all")
	time.Sleep(100 * time.Millisecond)

	n.human.say("GuildServ", "CATCHUP")

	n.human.expect("the catch-up", fromGuildServ("method"))
	if m := brain.lastCatchup(t).Model; m != "openai:gpt-6-luna" {
		t.Errorf("written on %q, want OpenAI's fast model", m)
	}
}

func TestCatchupForOneChannel(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain)
	dana := joinHuman(t, n.srv.Addr(), "dana", "#general", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
	dana.say("#general", "hi all")
	dana.say("#engineering", "the build is green again")
	time.Sleep(100 * time.Millisecond)

	n.human.say("GuildServ", "CATCHUP #engineering")

	n.human.expect("the catch-up", fromGuildServ("method"))
	req := brain.lastCatchup(t)
	if len(req.Channels) != 1 || req.Channels[0].Name != "#engineering" {
		t.Errorf("CATCHUP #engineering covered %v, want only #engineering", said(req))
	}
}

func TestCatchupOnlyCoversChannelsYoureIn(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain)
	dana := joinHuman(t, n.srv.Addr(), "dana", "#engineering")
	n.waitForMembers("#engineering", "dana", "kai", "ledger", "method", "vera")
	n.human.say("#general", "something in general")
	n.human.say("#engineering", "something in engineering")
	time.Sleep(100 * time.Millisecond)

	dana.say("GuildServ", "CATCHUP")

	dana.expect("the catch-up", fromGuildServ("dana"))
	got := said(brain.lastCatchup(t))
	if strings.Contains(got, "#general") {
		t.Errorf("dana isn't in #general, so it shouldn't be in her catch-up:\n%s", got)
	}
	if !strings.Contains(got, "something in engineering") {
		t.Errorf("dana's catch-up should cover #engineering:\n%s", got)
	}
}

func TestCatchupWithNothingNewSaysSoWithoutCallingTheModel(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain)

	n.human.say("GuildServ", "CATCHUP 1h")

	n.human.expect("a nothing-new reply", func(m heard) bool {
		return m.from == "GuildServ" && strings.Contains(strings.ToLower(m.text), "nothing new")
	})
	if c := brain.catchupCount(); c != 0 {
		t.Errorf("called the model %d times with nothing to summarize", c)
	}
}

func TestCatchupWindowCanBeChosen(t *testing.T) {
	brain := &fakeBrain{digest: "summary"}
	n := startNetwork(t, brain)
	n.human.say("#engineering", "just now")
	time.Sleep(100 * time.Millisecond)

	n.human.say("GuildServ", "CATCHUP 3h")

	n.human.expect("the catch-up", fromGuildServ("method"))
	req := brain.lastCatchup(t)
	if d := time.Since(req.Since); d < 179*time.Minute || d > 181*time.Minute {
		t.Errorf("CATCHUP 3h covered the last %v, want 3h", d.Round(time.Minute))
	}
}
