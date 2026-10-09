// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/runner"
)

// Each agent runs on its platform: plain chat, or a desk with tools on
// Managed Agents. The switch is per agent (SET <nick> PLATFORM).

func onPlatform(nick, platform string) option {
	return func(_ *config.Runner, ps *[]config.Persona) {
		for i := range *ps {
			if (*ps)[i].Nick == nick {
				(*ps)[i].Platform = platform
			}
		}
	}
}

func TestAgentsAreWokenOnTheirOwnPlatform(t *testing.T) {
	chat, desk := &fakeBrain{reply: replyWith("from chat")}, &fakeBrain{reply: replyWith("from my desk")}
	n := startNetworkWith(t, runner.NewRouter(chat, desk), onPlatform("kai", "managed"))

	n.human.say("#engineering", "kai: hi")
	n.human.expect("kai answering from its desk", fromContaining("kai", "#engineering", "from my desk"))
	n.human.say("#engineering", "vera: hi")
	n.human.expect("vera answering from chat", fromContaining("vera", "#engineering", "from chat"))
}

func TestSwitchingAnAgentsPlatformMovesIt(t *testing.T) {
	chat, desk := &fakeBrain{reply: replyWith("from chat")}, &fakeBrain{reply: replyWith("from my desk")}
	n := startNetworkWith(t, runner.NewRouter(chat, desk))

	n.human.say("GuildServ", "SET kai PLATFORM managed")
	n.human.expect("a confirmation", guildServSaying("method", "kai's platform is now managed"))
	n.human.say("#engineering", "kai: hi")

	n.human.expect("kai answering from its new desk", fromContaining("kai", "#engineering", "from my desk"))
	desk.mu.Lock()
	defer desk.mu.Unlock()
	if len(desk.staffing) == 0 || !strings.HasPrefix(desk.staffing[0], "update kai") {
		t.Errorf("the platform should hear about kai moving to it; heard %v", desk.staffing)
	}
}

// Desks are on Managed Agents, which runs Claude: an agent on another
// provider's model works in chat until it moves to a Claude model.
func TestDesksAreForClaudeModels(t *testing.T) {
	desk := &fakeBrain{reply: replyWith("from my desk")}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, desk), withProviders(models.Anthropic, models.OpenAI), onPlatform("vera", "managed"))

	n.human.say("GuildServ", "SET kai MODEL openai:gpt-6-sol")
	n.human.expect("kai on GPT-6", guildServSaying("method", "openai:gpt-6-sol"))
	n.human.say("GuildServ", "SET kai PLATFORM managed")
	n.human.expect("no desk for GPT-6", guildServSaying("method", "desks run Claude"))
	n.human.say("GuildServ", "SET vera MODEL openai:gpt-6-sol")
	n.human.expect("vera stays on Claude at her desk", guildServSaying("method", "desks run Claude"))
	n.human.say("GuildServ", "HIRE rex MODEL openai:gpt-6-luna PLATFORM managed")
	n.human.expect("rex refused a desk", guildServSaying("method", "desks run Claude"))
}

func TestManagedAgentsMustBeSetUpBeforeAgentsCanMoveThere(t *testing.T) {
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, nil))

	n.human.say("GuildServ", "SET kai PLATFORM managed")

	n.human.expect("a refusal", guildServSaying("method", "isn't set up"))
}

// Desks of GuildNet's own run the runner's own loop, so they take any model.
func TestAgentsCanWorkAtADeskOfGuildNetsOwnOnAnyModel(t *testing.T) {
	own := &fakeBrain{reply: replyWith("from my own desk")}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{reply: replyWith("from chat")}, nil).WithDesks(own),
		withProviders(models.Anthropic, models.OpenAI))

	n.human.say("GuildServ", "SET kai MODEL openai:gpt-6-sol")
	n.human.expect("kai on GPT-6", guildServSaying("method", "openai:gpt-6-sol"))
	n.human.say("GuildServ", "SET kai PLATFORM desk")
	n.human.expect("kai moved to a desk", guildServSaying("method", "kai's platform is now desk"))
	n.human.say("#engineering", "kai: hi")

	n.human.expect("kai answering from its desk", fromContaining("kai", "#engineering", "from my own desk"))
	n.human.say("GuildServ", "FIRE kai")
	n.human.expect("kai fired", guildServSaying("method", "fired kai"))
	own.mu.Lock()
	defer own.mu.Unlock()
	if !slices.Contains(own.staffing, "fire kai") {
		t.Errorf("the desk platform heard %v, want kai's desk cleared", own.staffing)
	}
}

// An agent moved off its desk has the desk cleared: nothing of it is left
// for a later agent of the same name, even if it's fired from chat.
func TestMovingAnAgentOffItsDeskClearsIt(t *testing.T) {
	own := &fakeBrain{reply: replyWith("from my own desk")}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{reply: replyWith("from chat")}, nil).WithDesks(own))
	n.human.say("GuildServ", "SET kai PLATFORM desk")
	n.human.expect("kai moved to a desk", guildServSaying("method", "kai's platform is now desk"))

	n.human.say("GuildServ", "SET kai PLATFORM chat")
	n.human.expect("kai back in chat", guildServSaying("method", "kai's platform is now chat"))

	own.mu.Lock()
	defer own.mu.Unlock()
	if !slices.Contains(own.staffing, "fire kai") {
		t.Errorf("the desk platform heard %v, want kai's desk cleared", own.staffing)
	}
}

// An agent moved to another team gets a fresh desk, with that team's repos,
// not the old team's.
func TestMovingAnAgentToAnotherTeamClearsItsDesk(t *testing.T) {
	own := &fakeBrain{reply: replyWith("from my own desk")}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{reply: replyWith("from chat")}, nil).WithDesks(own), onPlatform("kai", "desk"))

	n.human.say("GuildServ", "SET kai TEAM Marketing")
	n.human.expect("kai moved", guildServSaying("method", "kai's team is now Marketing"))

	own.mu.Lock()
	defer own.mu.Unlock()
	if !slices.Contains(own.staffing, "fire kai") {
		t.Errorf("the desk platform heard %v, want kai's desk cleared", own.staffing)
	}
}

func TestDesksMustBeSetUpBeforeAgentsCanMoveThere(t *testing.T) {
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, &fakeBrain{}))

	n.human.say("GuildServ", "SET kai PLATFORM desk")

	n.human.expect("a refusal", guildServSaying("method", "Desks aren't set up"))
}

func TestAnAgentAtADeskOfGuildNetsOwnSaysItsOnItAndGetsLonger(t *testing.T) {
	own := deskBrain{&fakeBrain{reply: replyWith("done, tests pass")}, 300 * time.Millisecond}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, nil).WithDesks(own), onPlatform("kai", "desk"),
		timeouts(100*time.Millisecond, 10*time.Second))

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("an acknowledgement", actionFrom("kai", "#engineering", "on it"))
	n.human.expect("kai answering after working longer than a chat reply may take", fromContaining("kai", "#engineering", "done, tests pass"))
}

// deskBrain takes a while to answer, like a desk working through tools.
type deskBrain struct {
	*fakeBrain
	delay time.Duration
}

func (b deskBrain) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
	return b.fakeBrain.Respond(ctx, req, onStep)
}

func timeouts(reply, desk time.Duration) option {
	return func(cfg *config.Runner, _ *[]config.Persona) {
		cfg.ReplyTimeout, cfg.DeskTimeout = reply, desk
	}
}

func TestAgentsAtADeskGetLongerToWorkThanAQuickReply(t *testing.T) {
	desk := deskBrain{&fakeBrain{reply: replyWith("done, tests pass")}, 300 * time.Millisecond}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, desk), onPlatform("kai", "managed"),
		timeouts(100*time.Millisecond, 10*time.Second))

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai answering after working longer than a chat reply may take", fromContaining("kai", "#engineering", "done, tests pass"))
}

func TestAnAgentAtADeskSaysItsOnItStraightAway(t *testing.T) {
	desk := deskBrain{&fakeBrain{reply: replyWith("PR #3 is up")}, 2 * time.Second}
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{reply: replyWith("hi")}, desk), onPlatform("kai", "managed"))

	n.human.say("#engineering", "kai: open a small PR")

	n.human.expect("an acknowledgement before the work is done", actionFrom("kai", "#engineering", "on it"))
	n.human.expect("then the reply", fromContaining("kai", "#engineering", "PR #3 is up"))
}

func TestAQuickChatReplyNeedsNoAcknowledgement(t *testing.T) {
	n := startNetworkWith(t, runner.NewRouter(&fakeBrain{reply: replyWith("hello")}, nil))

	n.human.say("#engineering", "kai: hi")

	n.human.expect("the reply", fromContaining("kai", "#engineering", "hello"))
	n.human.expectQuiet("an acknowledgement", 300*time.Millisecond, actionFrom("kai", "#engineering", "on it"))
}

// Whether desks can be had here, for the console's hire form to offer one.
func TestTheAPISaysWhetherDesksCanBeHad(t *testing.T) {
	with := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, &fakeBrain{}), withAPI())
	without := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, nil), withAPI())

	if _, out := with.api("method").call("GET", "/v1/github", nil); out["desks"] != true {
		t.Errorf("with Managed Agents: %v, want desks", out)
	}
	if _, out := without.api("method").call("GET", "/v1/github", nil); out["desks"] != false {
		t.Errorf("without: %v, want no desks", out)
	}
}

// Desks of GuildNet's own are said apart from Managed Agents', for the
// console to offer each where it can be had.
func TestTheAPISaysWhetherDesksOfGuildNetsOwnCanBeHad(t *testing.T) {
	with := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, nil).WithDesks(&fakeBrain{}), withAPI())
	without := startNetworkWith(t, runner.NewRouter(&fakeBrain{}, &fakeBrain{}), withAPI())

	if _, out := with.api("method").call("GET", "/v1/github", nil); out["own_desks"] != true || out["desks"] != false {
		t.Errorf("with a sandbox: %v, want own desks and no Managed Agents", out)
	}
	if _, out := without.api("method").call("GET", "/v1/github", nil); out["own_desks"] != false {
		t.Errorf("without a sandbox: %v", out)
	}
}
