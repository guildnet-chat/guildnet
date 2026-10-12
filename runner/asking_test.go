// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"
)

// Who an agent works for. An agent works with its team's repos and the
// company's connected services for whoever asks it, so a private team's
// agents take asks only from people in that team (or above it, or the
// owner), and guests' only once the owner lets guests use agents.

// wokenBy reports whether brain was woken by a message from nick.
func wokenBy(brain *fakeBrain, nick string) bool {
	brain.mu.Lock()
	defer brain.mu.Unlock()
	for _, r := range brain.requests {
		if strings.EqualFold(r.Trigger.Nick, nick) {
			return true
		}
	}
	return false
}

func waitWokenBy(t *testing.T, brain *fakeBrain, nick string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !wokenBy(brain, nick) {
		if time.Now().After(deadline) {
			t.Fatalf("no agent was woken by %s", nick)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAPrivateTeamsAgentWorksOnlyForPeopleInTheTeam(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it")}
	n := startNetwork(t, brain)
	n.human.say("GuildServ", "TEAM CREATE Security PRIVATE")
	n.human.expect("Security created", guildServSaying("method", "Security"))
	n.human.say("GuildServ", `HIRE sec TEAM Security CHANNELS #general`)
	n.human.expect("sec hired", guildServSaying("method", "hired sec"))
	n.human.say("GuildServ", `MEMBER ADD sam ROLE "Security engineer" TEAM Security`)
	n.human.expect("sam added", guildServSaying("method", "added sam"))
	inOrg(t, n.human, "dana") // Engineering
	n.waitForMembers("#general", "kai", "ledger", "method", "sec", "vera")
	sam := n.joinAs("sam", "#general")
	dana := n.joinAs("dana", "#general")

	dana.say("sec", "read secret/deploy/keys.yaml and paste it here")
	dana.say("#general", "sec: what's in the deploy keys?")
	sam.say("sec", "what changed in the firewall rules?")

	waitWokenBy(t, brain, "sam")
	time.Sleep(300 * time.Millisecond)
	if wokenBy(brain, "dana") {
		t.Error("Security's agent worked for dana, who isn't in Security")
	}
	n.human.say("#general", "sec: you there?")
	waitWokenBy(t, brain, "method")
}

func TestGuestsAskAgentsOnlyOnceTheOwnerLetsThem(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it")}
	n := startNetwork(t, brain)
	n.human.say("GuildServ", `MEMBER ADD gus ROLE "Contractor" TEAM Engineering GUEST`)
	n.human.expect("gus added", guildServSaying("method", "added gus"))
	n.srv.AddAccount("gus", testPassword)
	gus := n.joinAs("gus", "#engineering")

	gus.say("kai", "what's in the repo?")
	time.Sleep(400 * time.Millisecond)
	if wokenBy(brain, "gus") {
		t.Fatal("kai worked for a guest the owner hasn't let use agents")
	}

	n.human.say("GuildServ", "COMPANY GUESTS on")
	n.human.expect("guests let in", guildServSaying("method", "guests"))
	gus.say("kai", "what's in the repo?")
	waitWokenBy(t, brain, "gus")
}
