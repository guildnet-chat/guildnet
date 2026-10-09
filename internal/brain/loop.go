// SPDX-License-Identifier: AGPL-3.0-only

package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// Agents in chat use GuildNet's tools (the Toolbox) on any model: the
// brain calls the model with the tools, runs what it asks for, and sends
// the results back, until it answers. A tool that needs a person pauses
// the agent; the conversation so far rides in the approval's token, so it
// carries on from there once decided, even on another runner. Agents at
// desks of GuildNet's own work the same way, with more tools (desks.go).

const (
	// chatRounds caps the model calls in one wake-up in chat.
	chatRounds = 20
	// maxToolOutput caps what one tool call gives the model.
	maxToolOutput = 64 << 10
	// keptOutput is how much tool output a conversation keeps: past it, the
	// oldest is dropped, all but the latest round's.
	keptOutput = 256 << 10
	// replyTokens is the most one call may write.
	replyTokens = 8000
)

// resumeToken is where a paused agent was: the conversation, the results of
// the turn's other calls, and the call waiting on a person, as pinned for
// approval.
type resumeToken struct {
	Turns   []turn   `json:"turns"`
	Done    []turn   `json:"done,omitempty"`
	Pending toolCall `json:"pending"`
}

// loop is how an agent works with tools: which it has, what it's told about
// what it has, and how many rounds it may take.
type loop struct {
	b       *Brain
	toolbox agent.Toolbox
	kit     agent.Kit
	rounds  int
}

// chat is the loop for agents in chat.
func (b *Brain) chat() loop {
	return loop{b: b, toolbox: b.toolbox, kit: agent.Tools, rounds: chatRounds}
}

// Respond wakes the agent: a reply, after using its tools if it has any.
func (b *Brain) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	p := req.Persona
	l := b.chat()
	if len(l.tools(p)) == 0 {
		a, err := b.complete(ctx, p.Model, call{system: agent.SystemPrompt(b.company, b.owner, p, agent.NoTools),
			prompt: agent.WakePrompt(req), maxTokens: replyTokens, effort: p.Effort})
		return agent.Result{Reply: a.text, Usage: a.usage, Cost: a.cost}, err
	}
	return l.start(ctx, req, onStep)
}

// Resume answers the approval the agent was waiting on and carries on.
func (b *Brain) Resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error) {
	if b.toolbox == nil {
		if _, err := parseResume(token); err != nil {
			return agent.Result{}, err
		}
		return agent.Result{}, agent.ErrNoTools
	}
	return b.chat().resume(ctx, p, token, allow, reason, onStep)
}

func parseResume(token string) (resumeToken, error) {
	var t resumeToken
	if err := json.Unmarshal([]byte(token), &t); err != nil || len(t.Turns) == 0 {
		return t, errors.New("this approval can't be carried on: it wasn't raised by an agent in chat or at a desk of GuildNet's own")
	}
	return t, nil
}

// start works on a wake-up from the beginning.
func (l loop) start(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	return l.work(ctx, req.Persona, []turn{{Role: "user", Text: agent.WakePrompt(req)}}, steps(onStep))
}

// resume carries on from an approval.
func (l loop) resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error) {
	t, err := parseResume(token)
	if err != nil {
		return agent.Result{}, err
	}
	onStep = steps(onStep)
	result := turn{Role: "tool", CallID: t.Pending.ID}
	switch {
	case allow:
		onStep(agent.Step{Summary: l.step(t.Pending)})
		result = l.run(ctx, p, t.Pending, onStep)
	case reason != "":
		result.Text, result.IsError = "Not approved, so it wasn't done. Reason: "+reason, true
	default:
		result.Text, result.IsError = "Not approved, so it wasn't done. No reason was given.", true
	}
	return l.work(ctx, p, append(append(t.Turns, t.Done...), result), onStep)
}

func steps(onStep func(agent.Step)) func(agent.Step) {
	if onStep == nil {
		return func(agent.Step) {}
	}
	return onStep
}

func (l loop) tools(p config.Persona) []agent.Tool {
	if l.toolbox == nil {
		return nil
	}
	return l.toolbox.Tools(p)
}

// work calls the model, runs the tools it asks for, and carries on until it
// answers, pauses for a person, or has taken too many rounds.
func (l loop) work(ctx context.Context, p config.Persona, turns []turn, onStep func(agent.Step)) (res agent.Result, err error) {
	system := agent.SystemPrompt(l.b.company, l.b.owner, p, l.kit)
	if guide := l.toolbox.Guide(p); guide != "" {
		system += "\n\n" + guide
	}
	tools := l.tools(p)
	for round := 1; ; round++ {
		if round > l.rounds {
			return res, fmt.Errorf("%s used tools %d times in a row without finishing, so GuildNet stopped it there", p.Nick, l.rounds)
		}
		turns = trimmed(turns, keptOutput)
		a, err := l.b.complete(ctx, p.Model, call{system: system, turns: turns, tools: tools, maxTokens: replyTokens, effort: p.Effort})
		res.Usage.Input += a.usage.Input
		res.Usage.Output += a.usage.Output
		res.Usage.CacheRead += a.usage.CacheRead
		res.Usage.CacheWrite += a.usage.CacheWrite
		res.Cost += a.cost
		if err != nil {
			return res, err
		}
		if len(a.calls) == 0 {
			res.Reply = a.text
			return res, nil
		}
		turns = append(turns, turn{Role: "assistant", Text: a.text, Calls: a.calls, Raw: a.raw})
		results, pending, summary := l.answer(ctx, p, tools, a.calls, onStep)
		if pending != nil {
			token, err := json.Marshal(resumeToken{Turns: turns, Done: results, Pending: *pending})
			if err != nil {
				return res, err
			}
			res.Reply = a.text
			res.Approval = &agent.ApprovalRequest{Tool: pending.Name, Summary: summary, Resume: string(token)}
			return res, nil
		}
		turns = append(turns, results...)
	}
}

// trimmed drops the oldest tool output once there's more than keep of it,
// leaving the latest round's whole: the model is told it was dropped, and
// can look again if it needs to.
func trimmed(turns []turn, keep int) []turn {
	total := 0
	for _, t := range turns {
		if t.Role == "tool" {
			total += len(t.Text)
		}
	}
	if total <= keep {
		return turns
	}
	latest := len(turns)
	for latest > 0 && turns[latest-1].Role == "tool" {
		latest--
	}
	out := append([]turn(nil), turns...)
	for i := 0; i < latest && total > keep; i++ {
		if out[i].Role != "tool" || len(out[i].Text) < len(droppedOutput) {
			continue
		}
		total -= len(out[i].Text) - len(droppedOutput)
		out[i].Text = droppedOutput
	}
	return out
}

const droppedOutput = "[This output was removed to save room. Run the tool again if you still need it.]"

const oneAtATime = "Only one approval can be asked for at a time. Ask again once the other one is decided."

// answer runs the calls that need no one, and picks at most one to ask a
// person about (with its summary for the approver).
func (l loop) answer(ctx context.Context, p config.Persona, tools []agent.Tool, calls []toolCall, onStep func(agent.Step)) ([]turn, *toolCall, string) {
	var out []turn
	var pending *toolCall
	var summary string
	for _, c := range calls {
		tool, known := findTool(tools, c.Name)
		ask := known && !c.Bad && (tool.Ask || agent.MustAsk(l.toolbox, p, c.Name, c.Input))
		switch {
		case !known:
			out = append(out, failed(c, fmt.Sprintf("there's no tool called %s", c.Name)))
		case c.Bad:
			out = append(out, failed(c, "the input wasn't a JSON object matching the tool's schema; try again"))
		case ask && pending != nil:
			out = append(out, failed(c, oneAtATime))
		case ask:
			s, pinned, err := l.toolbox.Prepare(ctx, p, c.Name, c.Input)
			if err != nil {
				out = append(out, failed(c, err.Error()))
				continue
			}
			pc := c
			pc.Input = pinned
			pending, summary = &pc, s
		default:
			onStep(agent.Step{Summary: l.step(c)})
			out = append(out, l.run(ctx, p, c, onStep))
		}
	}
	return out, pending, summary
}

// run carries out a call. What a tool that changes things achieved is shown
// too, so people see it even if the agent stops before saying so.
func (l loop) run(ctx context.Context, p config.Persona, c toolCall, onStep func(agent.Step)) turn {
	out, err := l.toolbox.Run(ctx, p, c.Name, c.Input)
	if err != nil {
		return failed(c, err.Error())
	}
	if tool, _ := findTool(l.tools(p), c.Name); !tool.ReadOnly {
		if line := agent.Headline(out); line != "" {
			onStep(agent.Step{Summary: line, Result: true})
		}
	}
	return turn{Role: "tool", CallID: c.ID, Text: capped(out)}
}

func failed(c toolCall, msg string) turn {
	return turn{Role: "tool", CallID: c.ID, Text: msg, IsError: true}
}

func capped(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + fmt.Sprintf("\n[cut short: %d more bytes. Ask for less, e.g. a range of lines.]", len(s)-maxToolOutput)
}

func findTool(tools []agent.Tool, name string) (agent.Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return agent.Tool{}, false
}

// step describes a call for IRC, in the toolbox's words if it has them.
func (l loop) step(c toolCall) string {
	if s, ok := l.toolbox.(interface {
		Step(name string, input map[string]any) string
	}); ok {
		if text := s.Step(c.Name, c.Input); text != "" {
			return text
		}
	}
	return "uses " + c.Name
}
