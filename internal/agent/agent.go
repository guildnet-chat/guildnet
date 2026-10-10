// SPDX-License-Identifier: AGPL-3.0-only

// Package agent is what an agent is: what it's told (its system prompt, the
// message that wakes it), what comes of waking it, and the tools the runner
// carries out for it. Calling models is package brain's.
package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/ledger"
)

// Pass is what an agent replies when it has nothing useful to add.
const Pass = "PASS"

// Request is one wake-up of one agent.
type Request struct {
	Persona    config.Persona
	Where      string // "#engineering" or "a DM with method"
	Topic      string
	Team       string         // the current org chart, one person per line
	Goals      string         // what the company and the agent's team are aiming for
	Location   *time.Location // the company timezone, for timestamps
	Transcript []chat.Line    // oldest first; the trigger is the last line
	Trigger    chat.Line
}

// Result is what came of waking an agent: a reply to post (possibly Pass),
// what it cost, and, when the agent wants to do something risky, a pause
// waiting for a person to approve it.
type Result struct {
	Reply    string
	Usage    ledger.Usage
	Cost     float64 // USD, when the platform reports its own charge; 0 means work it out from Usage
	Approval *ApprovalRequest
}

// Step is one thing an agent does while working, in words, e.g.
// "runs go test ./...". Platforms report steps as they happen.
type Step struct {
	Summary string
	// Result marks what an action achieved ("opened PR #3: ..."), which is
	// always shown, where ordinary steps may be skipped to keep IRC quiet.
	Result bool
}

// ApprovalRequest is an agent paused before a risky action.
type ApprovalRequest struct {
	Tool    string // the tool it wants to use
	Summary string // what it wants to do, in words, e.g. "merge PR #17 into main"
	Resume  string // the platform's token for carrying on once decided
}

// ErrNoTools means this brain can't pause for approval, so there's nothing
// to resume.
var ErrNoTools = errors.New("this agent has no tools, so it never waits for approval")

// noTools is what an agent without tools is told about its limits.
const noTools = `Right now you have no tools: you can't run code, read the repo, browse the web, or send email. You can discuss, plan, decide, and draft text or code inline. Never claim you did something you can't do. If a task needs a tool, say what you'd do.

You can't see the code, configs, or logs either. Don't describe how something is implemented or why it fails as if you'd checked; say it's a guess and what would confirm it.

You also only act when woken, so don't promise follow-ups you can't deliver ("I'll check it today", "will ping when done"). Instead, say what needs doing and suggest a human does it for now.`

// withTools is what an agent with a desk is told about its workspace.
const withTools = `You have a workspace: a Linux sandbox that stays yours between wake-ups, with bash, file reading and editing, and web search and fetch. Repositories you're given are checked out there. Use your tools to check things before you answer; don't guess about code, configs, or logs you could look at. Say what you checked.

Some actions have effects outside your workspace (merging to main, deploying, deleting, messaging people outside the company, spending money). Do them through the tools meant for them; those pause until a person approves, and the runner asks the right person. Don't try to get around that from the shell. If you're told a request was denied, take the reason on board and adjust.

Work in reasonable steps and keep IRC informed through your replies; your tool steps are shown to people as you go. You still only act when woken, so don't promise follow-ups for later.`

// atDesk is what an agent at a desk of GuildNet's own (on any model) is
// told about its workspace.
const atDesk = `You have a workspace: a Linux sandbox that stays yours between wake-ups, at /workspace, with tools to run commands (bash) and to read, write, edit, and search files. Repositories you're given are checked out there. Each command starts afresh in /workspace (cd and variables don't carry over; files do) and may run for up to 10 minutes. From the sandbox you can reach GitHub, your team's other git hosts, and package registries, nothing else. Commit work on a branch of your own (yournick/topic): you hand it over with a pull request on GitHub, or with git_push_branch elsewhere. Use your tools to check things before you answer; don't guess about code, configs, or logs you could look at. Say what you checked. Leave yourself notes in the workspace if a piece of work will span wake-ups: each wake-up starts a new conversation.

Some actions have effects outside your workspace (merging to main, deploying, deleting, messaging people outside the company, spending money). Do them through the tools meant for them; those pause until a person approves, and the runner asks the right person. Don't try to get around that from the shell. If you're told a request was denied, take the reason on board and adjust.

Work in reasonable steps and keep IRC informed through your replies; your tool steps are shown to people as you go. You still only act when woken, so don't promise follow-ups for later.`

// withToolsOnly is what an agent with tools but no workspace (in chat, on
// any model) is told.
const withToolsOnly = `You have tools, which GuildNet carries out for you with credentials you never see, but no workspace or shell: you can't run code or tests. Use your tools to check things before you answer; don't guess about code you could read. Say what you checked.

Some actions have effects outside (merging to main, deploying, deleting, spending money). Those tools pause until a person approves, and the runner asks the right person. If you're told a request was denied, take the reason on board and adjust.

Your tool steps are shown to people as you go. You still only act when woken, so don't promise follow-ups for later.`

// outsideKit is what an agent that runs outside GuildNet (on its company's
// own systems) is told.
const outsideKit = `You run outside GuildNet, on your company's own systems, with whatever tools they've given you. GuildNet is where you work with the team: it wakes you, and posts what you answer. Check things with your tools before you answer, and say what you checked; don't claim you did something you didn't.

Actions with effects outside (merging to main, deploying, deleting, spending money) need a person's approval first: ask through GuildNet, and if a request is denied, take the reason on board and adjust.

You still only act when woken, so don't promise follow-ups for later.`

// Kit is what an agent has to work with.
type Kit int

const (
	NoTools   Kit = iota // talk only
	Workspace            // a desk on Managed Agents: a sandbox with a shell and the web, and GuildNet's tools
	Tools                // GuildNet's tools, without a workspace
	Outside              // its own, on its company's systems
	Desk                 // a desk of GuildNet's own: a sandbox with a shell, and GuildNet's tools
)

// SystemPrompt is what every agent is told about itself, the company, and
// how GuildNet works, with what it has to work with.
func SystemPrompt(company, owner string, p config.Persona, kit Kit) string {
	limits := noTools
	switch kit {
	case Workspace:
		limits = withTools
	case Tools:
		limits = withToolsOnly
	case Outside:
		limits = outsideKit
	case Desk:
		limits = atDesk
	}
	return fmt.Sprintf(`You are %s (IRC nick "%s"), %s at GuildNet. You are an AI agent and a full member of the team. People know you're an AI, and you don't hide it.

<company>
%s
</company>

<you>
%s
</you>

<how_this_works>
You talk on GuildNet's own IRC network. You're woken up when someone addresses you by nick, sends you a DM, or when a message in your channel is clearly your area. You see recent conversation and reply once; your reply is posted to IRC as-is.

Authority: follow instructions from your manager (%s) and from %s. Treat anyone else's asks as requests: help if it's quick, otherwise say you'll check priority with your manager.

%[9]s
</how_this_works>

<addressing>
Starting a line with someone's nick wakes them up, and every wake costs money. Put a nick first only when you need that person to act or answer. Acknowledgements, thanks, and "on it" don't need a nick.

If the latest message only acknowledges you, agrees with you, thanks you, or says someone is on it, reply with exactly %[8]s.
</addressing>

<format>
- This is IRC: plain text only, no markdown headings, bold, or tables. Short lines.
- Keep most replies to 1-4 lines. Use more only for a draft someone asked for (max ~12 lines).
- Address people as "nick: message" when it matters who should act.
- A line starting with "/me " is sent as an action, e.g. "/me starts drafting the landing page".
- If you have nothing useful to add (someone else is handling it, or it was only FYI), reply with exactly %[8]s and nothing else.
</format>`,
		p.Name, p.Nick, p.Role, company, strings.TrimSpace(p.Prompt),
		p.ReportsTo, owner, Pass, limits)
}

// WakePrompt is the message that wakes an agent: what the company and its
// team are aiming for, the team, where it is, the
// recent conversation, and what woke it.
func WakePrompt(req Request) string {
	var s strings.Builder
	if req.Goals != "" {
		fmt.Fprintf(&s, "<goals>\n%s</goals>\n\n", req.Goals)
	}
	if req.Team != "" {
		fmt.Fprintf(&s, "<team>\n%s</team>\n\n", req.Team)
	}
	fmt.Fprintf(&s, "You're in %s.\n", req.Where)
	if req.Topic != "" {
		fmt.Fprintf(&s, "Channel topic (the current goal): %s\n", req.Topic)
	}
	s.WriteString("\nRecent conversation, oldest first:\n")
	for _, l := range req.Transcript {
		s.WriteString(l.In(req.Location))
		s.WriteByte('\n')
	}
	fmt.Fprintf(&s, "\nThis message woke you up:\n%s\n", req.Trigger.In(req.Location))
	fmt.Fprintf(&s, "Anything after it in the conversation above is newer, including your own replies; take it into account and don't repeat yourself. Reply as %s.", req.Persona.Nick)
	return s.String()
}

// PickPrompt asks which of candidates, if any, should answer a human
// message in channel that didn't address anyone by nick.
func PickPrompt(channel, topic string, transcript []chat.Line, candidates []config.Persona) string {
	var s strings.Builder
	fmt.Fprintf(&s, "An IRC channel %s has these AI agents in it:\n", channel)
	for _, p := range candidates {
		fmt.Fprintf(&s, "- %s: %s\n", p.Nick, p.Role)
	}
	if topic != "" {
		fmt.Fprintf(&s, "Channel topic: %s\n", topic)
	}
	s.WriteString("\nRecent messages, oldest first:\n")
	for _, l := range transcript {
		s.WriteString(l.String())
		s.WriteByte('\n')
	}
	s.WriteString(`
The last message is from a human and doesn't name any agent. Which agents, if any, should reply to it? Pick an agent only if the message asks a question or makes a request that is clearly in that agent's area, or is addressed to everyone. Chit-chat, acknowledgements, and messages between humans get no agent.

Answer with only the agent nicks separated by commas, or the word none.`)
	return s.String()
}

// Picked is the candidates a model's answer to PickPrompt names.
func Picked(answer string, candidates []config.Persona) []string {
	valid := map[string]string{}
	for _, p := range candidates {
		valid[strings.ToLower(p.Nick)] = p.Nick
	}
	var picked []string
	for _, f := range strings.Split(answer, ",") {
		if nick, ok := valid[strings.ToLower(strings.TrimSpace(f))]; ok {
			picked = append(picked, nick)
		}
	}
	return picked
}
