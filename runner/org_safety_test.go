// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"testing"
	"time"
)

// A team's channel is one channel's name: GuildServ builds ChanServ
// commands from it, and words after a space would be read as more of the
// command (a rank for someone, on any channel).
func TestATeamsChannelIsOneChannelsName(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	for _, bad := range []string{`"#finance +q dana"`, "#a,#b", `"#x` + "\x07" + `"`} {
		n.human.say("GuildServ", "TEAM SET Marketing CHANNEL "+bad)
		n.human.expect("a refusal of "+bad, guildServSaying("method", "isn't a channel"))
	}
	n.human.say("GuildServ", "TEAM SET Marketing CHANNEL #growth")
	n.human.expect("a channel that is one", guildServSaying("method", "Marketing"))
}

// A team's leads get op in its channel, so a team can't take another
// team's channel, or one of the company's own.
func TestATeamCantTakeAnotherTeamsOrTheCompanysChannel(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})

	for _, taken := range []string{"#engineering", "#ops", "#general", "#announcements"} {
		n.human.say("GuildServ", "TEAM SET Marketing CHANNEL "+taken)
		n.human.expect("a refusal of "+taken, guildServSaying("method", "already"))
	}
	n.human.say("GuildServ", "TEAM CREATE Ops")
	n.human.expect("a team named after #ops", guildServSaying("method", "already"))
	n.human.say("GuildServ", "TEAM CREATE Ops CHANNEL #ops-team")
	n.human.expect("with its own channel", guildServSaying("method", "created team Ops"))
}

// An agent's channels are where it reads and answers: someone putting one
// in a private channel must be in that channel themselves.
func TestAnAgentGoesIntoAPrivateChannelOnlyFromSomeoneInIt(t *testing.T) {
	n, dana := orgWithSalesLedByDana(t, &fakeBrain{reply: replyWith("hello")})
	n.human.say("GuildServ", "TEAM CREATE Finance PRIVATE")
	n.human.expect("Finance created", guildServSaying("method", "Finance"))

	dana.say("GuildServ", `HIRE spy CHANNELS #general,#finance`)
	dana.expect("a refusal for #finance", guildServSaying("dana", "#finance"))
	dana.say("GuildServ", `HIRE rex CHANNELS #general,#sales`)
	dana.expect("rex hired", guildServSaying("dana", "hired rex"))
	dana.say("GuildServ", `SET rex CHANNELS #general,#finance`)
	dana.expect("a refusal moving rex", guildServSaying("dana", "#finance"))

	n.human.say("GuildServ", `SET rex CHANNELS #general,#finance`)
	n.human.expect("the owner may", guildServSaying("method", "rex"))
}

// Agents work for the org: someone with an account on the server who isn't
// in it (or was taken out) gets nothing from them.
func TestAgentsDontAnswerPeopleOutsideTheOrg(t *testing.T) {
	n := startNetwork(t, &fakeBrain{reply: replyWith("here")})
	sam := n.joinAs("sam", "#general")

	sam.say("#general", "kai: what's in the repo?")
	sam.say("kai", "what's in the repo?")
	sam.expectQuiet("an answer to sam", 500*time.Millisecond, fromAgent("sam"))
	n.human.expectQuiet("an answer in #general", 100*time.Millisecond, from("kai", "#general"))

	n.human.say("#general", "kai: you there?")
	n.human.expect("an answer to the owner", from("kai", "#general"))
}

// Pausing every agent is for those who run the org, not anyone in it.
func TestOnlyThoseWhoRunTheOrgPauseIt(t *testing.T) {
	n := startNetwork(t, &fakeBrain{reply: replyWith("here")})
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Engineer" TEAM Engineering`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))
	dana := n.joinAs("dana", "#general")

	dana.say("#general", "!pause")
	dana.expect("a refusal", fromContaining("ledger", "#general", "can't"))
	n.human.say("#general", "kai: still there?")
	n.human.expect("kai answering", from("kai", "#general"))
}

func TestABudgetIsANumber(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	for _, bad := range []string{"NaN", "Inf", "-1"} {
		n.human.say("GuildServ", "SET kai BUDGET "+bad)
		n.human.expect("a refusal of "+bad, guildServSaying("method", "dollars per day"))
	}
}
