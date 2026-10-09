// SPDX-License-Identifier: AGPL-3.0-only

// Package managed runs GuildNet agents on Anthropic's Managed Agents: each
// agent gets an agent config there and a long-lived session (its desk, with a
// sandboxed workspace), and this package turns the desk's events into what
// the runner understands. It's the beta's agent platform; the long-term
// replacement (our own loop and sandbox, for BYOK) implements the same
// runner.Brain interface.
//
// Tools that reach outside the workspace (GitHub) are custom tools: the desk
// asks for them, and this package runs them through an agent.Toolbox, with
// credentials the agent never sees, pausing for a person where the tool needs
// approval.
package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
)

// EventKind is what happened on a desk.
type EventKind int

const (
	Message EventKind = iota // the agent said something
	ToolUse                  // the agent used a tool
	Usage                    // a snapshot of the desk's running cost
	Idle                     // the agent stopped: see Stop
	Error                    // something went wrong
)

// Event is one thing that happened on a desk, reduced to what we use.
type Event struct {
	Kind      EventKind
	ID        string         // the platform's event id (tool uses)
	Text      string         // Message text, or Error detail
	Tool      string         // tool name
	MCPServer string         // set for MCP tools, e.g. "github"
	Custom    bool           // one of our tools: we run it and send the result
	Input     map[string]any // tool input
	Ask       bool           // a built-in tool waiting for a person to approve it
	CostCents int64          // Usage: the desk's cumulative list cost
	Stop      string         // Idle: end_turn, requires_action, budget_reached, retries_exhausted
	Waiting   []string       // Idle requires_action: event ids waiting on us
}

// Answer is what we send back for a tool the desk is waiting on: a custom
// tool's result, or a decision on a built-in tool.
type Answer struct {
	EventID string `json:"id"`
	Result  bool   `json:"result,omitempty"` // a custom tool's result; otherwise a confirmation
	Allow   bool   `json:"allow,omitempty"`  // confirmation: go ahead
	Text    string `json:"text,omitempty"`   // the result, or why it was denied
	IsError bool   `json:"error,omitempty"`  // the result is a failure
}

// Desks is the narrow slice of the platform this package needs. The real one
// talks to the Managed Agents API; tests use a scripted fake.
type Desks interface {
	// EnsureAgent creates or updates the platform's config for p and returns
	// its id and version.
	EnsureAgent(ctx context.Context, p config.Persona, system string, tools []agent.Tool) (id string, version int64, err error)
	// NewDesk starts a long-lived session for the agent.
	NewDesk(ctx context.Context, p config.Persona, agentID string, version int64) (sessionID string, err error)
	// Send wakes p's desk with text and returns what happened until it
	// stopped. live hears each event as it happens.
	Send(ctx context.Context, p config.Persona, session, text string, live func(Event)) ([]Event, error)
	// Answer replies to tools the desk is waiting on, all at once, and
	// returns what happened until it stopped again.
	Answer(ctx context.Context, p config.Persona, session string, answers []Answer, live func(Event)) ([]Event, error)
	// Archive retires the agent's config.
	Archive(ctx context.Context, agentID string) error
}

// Options configure the brain.
type Options struct {
	Company string // shared company context for every agent
	Owner   string
	Toolbox agent.Toolbox // tools the runner carries out for agents; may be nil
}

// Brain runs agents on desks. It implements the runner's Brain and
// Staffing interfaces.
type Brain struct {
	desks Desks
	store org.Desks
	opts  Options
	mu    sync.Mutex // one desk operation per agent at a time is the runner's job; this guards the maps
}

// maxRounds caps how many rounds of tool calls one wake-up can take.
const maxRounds = 50

// New returns a Brain whose desks are remembered in store.
func New(desks Desks, store org.Desks, opts Options) *Brain {
	return &Brain{desks: desks, store: store, opts: opts}
}

// Respond wakes the agent at its desk.
func (b *Brain) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	d, err := b.desk(ctx, req.Persona)
	if err != nil {
		return agent.Result{}, err
	}
	events, err := b.desks.Send(ctx, req.Persona, d.SessionID, agent.WakePrompt(req), live(onStep))
	if err != nil {
		return agent.Result{}, err
	}
	return b.work(ctx, req.Persona, d, events, onStep)
}

// resumeToken is what an approval carries, so the agent can carry on after
// the decision, even across a restart.
type resumeToken struct {
	Session string   `json:"session"`
	Pending pending  `json:"pending"`
	Done    []Answer `json:"done,omitempty"` // answers for the turn's other tools, sent with the decision
}

// pending is the tool call waiting on a person.
type pending struct {
	EventID string         `json:"id"`
	Custom  bool           `json:"custom,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	Input   map[string]any `json:"input,omitempty"` // as pinned when it was prepared
}

func parseToken(token string) (resumeToken, error) {
	var t resumeToken
	if strings.HasPrefix(token, "{") {
		if err := json.Unmarshal([]byte(token), &t); err != nil {
			return t, fmt.Errorf("bad resume token: %w", err)
		}
		return t, nil
	}
	// Before custom tools, a token was "session/event".
	session, id, ok := strings.Cut(token, "/")
	if !ok {
		return t, fmt.Errorf("bad resume token %q", token)
	}
	return resumeToken{Session: session, Pending: pending{EventID: id}}, nil
}

// Resume answers the approval the agent was waiting on and carries on.
func (b *Brain) Resume(ctx context.Context, p config.Persona, token string, allow bool, reason string, onStep func(agent.Step)) (agent.Result, error) {
	t, err := parseToken(token)
	if err != nil {
		return agent.Result{}, err
	}
	d, err := b.store.Desk(ctx, p.Nick)
	if err != nil || d.SessionID != t.Session {
		return agent.Result{}, fmt.Errorf("%s's desk has changed since it asked for approval, so there's nothing to resume", p.Nick)
	}
	answers := t.Done
	switch pd := t.Pending; {
	case !pd.Custom:
		answers = append(answers, Answer{EventID: pd.EventID, Allow: allow, Text: reason})
	case allow:
		onStep(agent.Step{Summary: b.step(Event{Tool: pd.Tool, Custom: true, Input: pd.Input})})
		answers = append(answers, b.run(ctx, p, pd.EventID, pd.Tool, pd.Input, onStep))
	default:
		msg := "Not approved, so it wasn't done. Reason: " + reason
		if reason == "" {
			msg = "Not approved, so it wasn't done. No reason was given."
		}
		answers = append(answers, Answer{EventID: pd.EventID, Result: true, IsError: true, Text: msg})
	}
	events, err := b.desks.Answer(ctx, p, t.Session, answers, live(onStep))
	if err != nil {
		return agent.Result{}, err
	}
	return b.work(ctx, p, d, events, onStep)
}

// Hire gets the agent's config ready on the platform.
func (b *Brain) Hire(ctx context.Context, p config.Persona) error {
	_, err := b.desk(ctx, p)
	return err
}

// Update refreshes the agent's config; its next wake-up gets a new desk if
// the config changed.
func (b *Brain) Update(ctx context.Context, p config.Persona) error {
	_, err := b.desk(ctx, p)
	return err
}

// Fire retires the agent's config on the platform.
func (b *Brain) Fire(ctx context.Context, p config.Persona) error {
	d, err := b.store.Desk(ctx, p.Nick)
	if errors.Is(err, org.ErrNotFound) {
		return nil // never had a desk
	}
	if err != nil {
		return err
	}
	return b.desks.Archive(ctx, d.AgentID)
}

// desk returns the agent's desk, making its config and session as needed.
// A changed config (a new version) gets a new desk, because a session stays
// on the version it started with.
func (b *Brain) desk(ctx context.Context, p config.Persona) (org.Desk, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	system := agent.SystemPrompt(b.opts.Company, b.opts.Owner, p, agent.Workspace)
	var tools []agent.Tool
	if b.opts.Toolbox != nil {
		if guide := b.opts.Toolbox.Guide(p); guide != "" {
			system += "\n\n" + guide
		}
		tools = b.opts.Toolbox.Tools(p)
	}
	id, version, err := b.desks.EnsureAgent(ctx, p, system, tools)
	if err != nil {
		return org.Desk{}, fmt.Errorf("setting up %s on the agent platform: %w", p.Nick, err)
	}
	d, err := b.store.Desk(ctx, p.Nick)
	if err == nil && d.AgentID == id && d.AgentVersion == version && d.SessionID != "" {
		return d, nil
	}
	if err != nil && !errors.Is(err, org.ErrNotFound) {
		return org.Desk{}, err
	}
	session, err := b.desks.NewDesk(ctx, p, id, version)
	if err != nil {
		return org.Desk{}, fmt.Errorf("opening %s's desk: %w", p.Nick, err)
	}
	d = org.Desk{Nick: p.Nick, AgentID: id, AgentVersion: version, SessionID: session}
	return d, b.store.SaveDesk(ctx, d)
}

// work turns a desk's events into a runner result, running the tools the
// desk waits on and carrying on until the agent finishes its turn or needs a
// person: steps as they come, the last thing the agent said as the reply, a
// pause for approval, and the cost since last time.
func (b *Brain) work(ctx context.Context, p config.Persona, d org.Desk, events []Event, onStep func(agent.Step)) (res agent.Result, err error) {
	start := d.CostCents
	charged := d.CostCents // what the wake-up's meter has been charged up to
	meter := agent.MeterOf(ctx)
	spent := false // the desk used its lifetime budget, so it's retired
	defer func() {
		if d.CostCents > start {
			res.Cost = float64(d.CostCents-start) / 100
		}
		if spent {
			d.SessionID = "" // the next wake-up opens a new desk
		}
		if d.CostCents > start || spent {
			if serr := b.store.SaveDesk(ctx, d); serr != nil && err == nil {
				err = serr
			}
		}
	}()
	for round := 1; ; round++ {
		calls := map[string]Event{}
		var stop string
		var waiting []string
		for _, e := range events {
			switch e.Kind {
			case Message:
				if t := strings.TrimSpace(e.Text); t != "" {
					res.Reply = t
				}
			case ToolUse:
				calls[e.ID] = e
			case Usage:
				d.CostCents = max(d.CostCents, e.CostCents)
			case Idle:
				stop, waiting = e.Stop, e.Waiting
			case Error:
				res.Reply = ""
				return res, fmt.Errorf("the agent platform reported a problem: %s", e.Text)
			}
		}
		switch stop {
		case "requires_action":
		case "budget_reached":
			spent = true
			return res, fmt.Errorf("%s's desk used its budget and stopped; the next wake-up starts a fresh desk", d.Nick)
		case "retries_exhausted":
			return res, errors.New("the agent platform gave up after repeated errors")
		default:
			return res, nil
		}
		if round > maxRounds {
			return res, fmt.Errorf("%s used tools %d times in a row without finishing, so GuildNet stopped it there", p.Nick, maxRounds)
		}
		// The desk's spend so far is charged, and it's sent on only while
		// the budget allows.
		if meter != nil {
			if d.CostCents > charged {
				meter.Charge(p.Model, ledger.Usage{}, float64(d.CostCents-charged)/100)
				charged = d.CostCents
			}
			if err := meter.Allow(); err != nil {
				return res, err
			}
		}
		answers, pend, summary := b.answer(ctx, p, waiting, calls, onStep)
		if pend != nil {
			token, err := json.Marshal(resumeToken{Session: d.SessionID, Pending: *pend, Done: answers})
			if err != nil {
				return res, err
			}
			res.Approval = &agent.ApprovalRequest{Tool: pend.Tool, Summary: summary, Resume: string(token)}
			return res, nil
		}
		if len(answers) == 0 {
			return res, errors.New("the agent is waiting on something GuildNet doesn't handle yet")
		}
		if events, err = b.desks.Answer(ctx, p, d.SessionID, answers, live(onStep)); err != nil {
			return res, err
		}
	}
}

// live shows the desk's own tool steps as they happen. Our tools' steps are
// shown when we run them.
func live(onStep func(agent.Step)) func(Event) {
	return func(e Event) {
		if e.Kind == ToolUse && !e.Ask && !e.Custom {
			onStep(agent.Step{Summary: Summarize(e)})
		}
	}
}

const oneAtATime = "Only one approval can be asked for at a time. Ask again once the other one is decided."

// answer works out what to send back for the tools the desk is waiting on:
// it runs the tools that don't need approval, and picks at most one call to
// ask a person about (with its summary for the approver).
func (b *Brain) answer(ctx context.Context, p config.Persona, waiting []string, calls map[string]Event, onStep func(agent.Step)) ([]Answer, *pending, string) {
	var out []Answer
	var pend *pending
	var summary string
	for _, id := range waiting {
		e, ok := calls[id]
		if !ok {
			continue
		}
		if !e.Custom { // a built-in tool the platform wants approved
			if !e.Ask {
				continue
			}
			if pend != nil {
				out = append(out, Answer{EventID: id, Text: oneAtATime})
				continue
			}
			pend, summary = &pending{EventID: id, Tool: toolName(e)}, Summarize(e)
			continue
		}
		tool, ok := b.tool(p, e.Tool)
		ask := ok && (tool.Ask || agent.MustAsk(b.opts.Toolbox, p, e.Tool, e.Input))
		switch {
		case !ok:
			out = append(out, failed(id, fmt.Sprintf("there's no tool called %s", e.Tool)))
		case ask && pend != nil:
			out = append(out, failed(id, oneAtATime))
		case ask:
			s, pinned, err := b.opts.Toolbox.Prepare(ctx, p, e.Tool, e.Input)
			if err != nil {
				out = append(out, failed(id, err.Error()))
				continue
			}
			pend, summary = &pending{EventID: id, Custom: true, Tool: e.Tool, Input: pinned}, s
		default:
			onStep(agent.Step{Summary: b.step(e)})
			out = append(out, b.run(ctx, p, id, e.Tool, e.Input, onStep))
		}
	}
	return out, pend, summary
}

// run carries out a custom tool call and returns the answer for the desk.
// What it achieved is shown too, so people see it even if the agent stops
// before saying so.
func (b *Brain) run(ctx context.Context, p config.Persona, id, name string, input map[string]any, onStep func(agent.Step)) Answer {
	if b.opts.Toolbox == nil {
		return failed(id, fmt.Sprintf("there's no tool called %s", name))
	}
	out, err := b.opts.Toolbox.Run(ctx, p, name, input)
	if err != nil {
		return failed(id, err.Error())
	}
	// What a tool that only looks found isn't news for IRC.
	if tool, _ := b.tool(p, name); !tool.ReadOnly {
		if line := agent.Headline(out); line != "" {
			onStep(agent.Step{Summary: line, Result: true})
		}
	}
	return Answer{EventID: id, Result: true, Text: out}
}

func failed(id, msg string) Answer {
	return Answer{EventID: id, Result: true, IsError: true, Text: msg}
}

func (b *Brain) tool(p config.Persona, name string) (agent.Tool, bool) {
	if b.opts.Toolbox == nil {
		return agent.Tool{}, false
	}
	for _, t := range b.opts.Toolbox.Tools(p) {
		if t.Name == name {
			return t, true
		}
	}
	return agent.Tool{}, false
}

// step describes a custom tool call for IRC, in the toolbox's words if it
// has them.
func (b *Brain) step(e Event) string {
	if s, ok := b.opts.Toolbox.(interface {
		Step(name string, input map[string]any) string
	}); ok {
		if text := s.Step(e.Tool, e.Input); text != "" {
			return text
		}
	}
	return Summarize(e)
}

func toolName(e Event) string {
	if e.MCPServer != "" {
		return e.MCPServer + "." + e.Tool
	}
	return e.Tool
}

// Summarize describes a tool use in a few words, for IRC.
func Summarize(e Event) string {
	str := func(k string) string {
		v, _ := e.Input[k].(string)
		return v
	}
	short := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > n {
			return s[:n-1] + "…"
		}
		return s
	}
	file := func() string {
		p := str("file_path")
		if p == "" {
			p = str("path")
		}
		return strings.TrimPrefix(p, "/workspace/")
	}
	if e.MCPServer != "" {
		detail := describeInput(e.Input)
		if detail != "" {
			return fmt.Sprintf("uses %s: %s (%s)", e.MCPServer, e.Tool, detail)
		}
		return fmt.Sprintf("uses %s: %s", e.MCPServer, e.Tool)
	}
	if e.Custom {
		if detail := describeInput(e.Input); detail != "" {
			return fmt.Sprintf("uses %s (%s)", e.Tool, detail)
		}
		return "uses " + e.Tool
	}
	switch e.Tool {
	case "bash":
		return "runs " + short(str("command"), 120)
	case "read":
		return "reads " + file()
	case "write":
		return "writes " + file()
	case "edit":
		return "edits " + file()
	case "glob", "grep":
		if pat := str("pattern"); pat != "" {
			return "searches files for " + short(pat, 60)
		}
		return "searches files"
	case "web_search":
		return "searches the web for " + short(str("query"), 80)
	case "web_fetch":
		return "reads " + short(str("url"), 100)
	}
	if f := file(); f != "" {
		return e.Tool + " " + path.Clean(f)
	}
	return "uses " + e.Tool
}

// describeInput is a short, stable rendering of a tool's input.
func describeInput(in map[string]any) string {
	var parts []string
	for _, k := range []string{"owner", "repo", "number", "pullNumber", "pull_number", "title", "branch", "base", "head"} {
		if v, ok := in[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %v", k, v))
		}
	}
	return strings.Join(parts, ", ")
}

// Hash identifies an agent config, so the platform side can tell whether it
// changed. Exported for the SDK-backed Desks.
func Hash(p config.Persona, system string, tools []agent.Tool) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", p.Model, p.Effort, system)
	for _, t := range tools {
		schema, _ := json.Marshal(t.Properties)
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%v\x00%t", t.Name, t.Description, schema, t.Required, t.Ask)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
