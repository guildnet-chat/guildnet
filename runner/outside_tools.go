// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/org"
)

// An outside agent acts back through GuildNet's tools: posting where it
// belongs, reading the conversation there, asking for approval, and the
// network's own tools (GitHub, as agents in chat have it). A risky tool
// waits for a person, and GuildNet runs exactly what was approved; either
// way the agent is woken with the decision. The same tools are REST calls
// and an MCP server, for agent frameworks to load.

const (
	// agentMessagesAMinute caps what an outside agent posts.
	agentMessagesAMinute = 20
	// maxContextLines is the most conversation one get_context gives.
	maxContextLines = 200
)

// UseTools gives outside agents the network's own tools (GitHub's, without a
// workspace); nil for none.
func (r *Runner) UseTools(tb agent.Toolbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolbox = tb
}

func (r *Runner) tools() agent.Toolbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.toolbox
}

// builtinTools are GuildNet's own tools for outside agents.
var builtinTools = []agent.Tool{
	{Name: "post_message", Description: "Post a message as yourself: in a channel you're in, or in the DM you were woken in (dm:<nick>).",
		Properties: map[string]any{
			"where": map[string]any{"type": "string", "description": "A channel you're in, like #engineering, or dm:<nick> for the DM you were woken in"},
			"text":  map[string]any{"type": "string", "description": "What to say; plain text, short lines"},
		}, Required: []string{"where", "text"}},
	{Name: "show_step", Description: "Show a progress step on the wake-up you're working on, like \"checks the build\": people see it as you work, and it counts in the summary of your work.",
		Properties: map[string]any{"text": map[string]any{"type": "string", "description": "What you're doing, as an action"}}, Required: []string{"text"}, ReadOnly: true},
	{Name: "get_context", Description: "Read recent conversation in a channel you're in.",
		Properties: map[string]any{
			"where": map[string]any{"type": "string", "description": "A channel you're in"},
			"lines": map[string]any{"type": "integer", "description": "How many recent lines, up to 200 (default 50)"},
		}, Required: []string{"where"}, ReadOnly: true},
	{Name: "request_approval", Description: "Ask a person to approve something before you do it (deploying, spending, deleting). You're told at once it's waiting, and woken later with the decision. GuildNet records the ask and the answer; honour a denial.",
		Properties: map[string]any{
			"summary": map[string]any{"type": "string", "description": "What you want to do, as a verb phrase: \"deploy 1.4 to production\""},
			"where":   map[string]any{"type": "string", "description": "The channel it's about; defaults to where you were woken, or your team's channel"},
		}, Required: []string{"summary"}},
}

// agentTools are the tools p has: GuildNet's own, and the network's.
func (r *Runner) agentTools(p config.Persona) []agent.Tool {
	out := append([]agent.Tool(nil), builtinTools...)
	if tb := r.tools(); tb != nil {
		out = append(out, tb.Tools(p)...)
	}
	return out
}

// toolResult is what a tool gave: its output, or an approval it's waiting on.
type toolResult struct {
	Output   string
	Approval *org.Approval
}

// errAgentsPaused is what an outside agent hears while the network's agents
// are paused: it may not act until they're resumed.
var errAgentsPaused = refuse(conflict, "all agents are paused; you can act again once someone resumes them (!resume).")

// agentUse runs one of p's tools, named name, with input.
func (r *Runner) agentUse(ctx context.Context, p config.Persona, name string, in map[string]any) (toolResult, error) {
	if r.isPaused() {
		return toolResult{}, errAgentsPaused
	}
	str := func(k string) string { s, _ := in[k].(string); return strings.TrimSpace(s) }
	switch name {
	case "post_message":
		return toolResult{Output: "Posted."}, r.agentPost(p, str("where"), str("text"))
	case "show_step":
		return toolResult{Output: "Shown."}, r.agentShowStep(p, str("text"))
	case "get_context":
		lines, _ := in["lines"].(float64)
		got, err := r.agentContext(p, str("where"), int(lines))
		if err != nil {
			return toolResult{}, err
		}
		var b strings.Builder
		for _, l := range got {
			fmt.Fprintf(&b, "[%s] <%s> %s\n", l.Time.In(r.loc()).Format("15:04"), l.From, l.Text)
		}
		return toolResult{Output: strings.TrimSpace(b.String())}, nil
	case "request_approval":
		a, err := r.agentAsk(p, str("summary"), str("where"), "", nil)
		if err != nil {
			return toolResult{}, err
		}
		return toolResult{Approval: &a}, nil
	}
	tb := r.tools()
	var tool agent.Tool
	found := false
	if tb != nil {
		for _, t := range tb.Tools(p) {
			if t.Name == name {
				tool, found = t, true
			}
		}
	}
	if !found {
		return toolResult{}, refuse(notFound, "there's no tool called %s; GET /agents/v1/tools lists yours.", name)
	}
	if tool.Ask || agent.MustAsk(tb, p, name, in) {
		summary, pinned, err := tb.Prepare(ctx, p, name, in)
		if err != nil {
			return toolResult{}, refuse(invalid, "%v", err)
		}
		a, err := r.agentAsk(p, summary, str("where"), name, pinned)
		if err != nil {
			return toolResult{}, err
		}
		return toolResult{Approval: &a}, nil
	}
	if s, ok := tb.(interface {
		Step(name string, input map[string]any) string
	}); ok {
		if step := s.Step(name, in); step != "" {
			r.agentShowStep(p, step)
		}
	}
	out, err := tb.Run(ctx, p, name, in)
	if err != nil {
		return toolResult{}, refuse(invalid, "%v", err)
	}
	return toolResult{Output: out}, nil
}

// agentPost posts text as p: in a channel it's in, or the DM it was woken in.
func (r *Runner) agentPost(p config.Persona, where, text string) error {
	if r.isPaused() {
		return errAgentsPaused
	}
	if where == "" || text == "" {
		return refuse(invalid, "say where (a channel, or dm:<nick>) and what.")
	}
	if !r.outside.post(p.Nick, r.now()) {
		return refuse(tooMany, "that's %d messages this minute, the most an agent may post; wait a moment.", agentMessagesAMinute)
	}
	if peer, ok := strings.CutPrefix(where, "dm:"); ok {
		if !r.outside.wokenInDM(p.Nick, peer) {
			return refuse(forbidden, "you can only write to %s in the DM you were woken in.", peer)
		}
		r.deliver(p, "", peer, chat.DMKey(p.Nick, peer), text, "")
		return nil
	}
	if !isChannel(where) || !r.inChannel(p, where) {
		return refuse(forbidden, "you're not in %s; post where you are: %s.", where, strings.Join(r.channelsFor(p), ", "))
	}
	r.deliver(p, where, where, where, text, "")
	return nil
}

// agentShowStep shows a step on the wake-up p is working on.
func (r *Runner) agentShowStep(p config.Persona, text string) error {
	text = strings.Join(strings.Fields(text), " ")
	x := r.outside.current(p.Nick)
	if x == nil {
		return refuse(conflict, "you've no wake-up open to show a step on.")
	}
	if text != "" {
		x.onStep(agent.Step{Summary: text})
	}
	return nil
}

// agentContext is recent conversation in a channel p is in.
func (r *Runner) agentContext(p config.Persona, where string, lines int) ([]wakeLine, error) {
	if !isChannel(where) || !r.inChannel(p, where) {
		return nil, refuse(forbidden, "you're not in %s.", where)
	}
	if lines <= 0 {
		lines = 50
	}
	out := []wakeLine{}
	for _, l := range r.chat.Recent(where, min(lines, maxContextLines)) {
		out = append(out, wakeLineOf(l))
	}
	return out, nil
}

// outsideResume is what an outside agent's approval carries: the tool to
// run once it's approved, as the approver saw it, if any.
type outsideResume struct {
	Outside bool           `json:"outside"`
	Tool    string         `json:"tool,omitempty"`
	Input   map[string]any `json:"input,omitempty"`
}

// agentAsk raises an approval for p, about where (or where it was woken,
// or its team's channel); tool and pinned, if set, run once approved.
func (r *Runner) agentAsk(p config.Persona, summary, where, tool string, pinned map[string]any) (org.Approval, error) {
	summary = strings.Join(strings.Fields(summary), " ")
	if summary == "" {
		return org.Approval{}, refuse(invalid, "say what you want to do, as a verb phrase: \"deploy 1.4 to production\".")
	}
	if len(summary) > 300 {
		return org.Approval{}, refuse(invalid, "keep the summary under 300 characters.")
	}
	// One at a time: the check and the asking are one step, so calls at
	// once can't each raise one.
	k := strings.ToLower(p.Nick)
	r.mu.Lock()
	a, waiting := r.wait.on[k]
	asking := r.wait.asking[k]
	if !waiting && !asking {
		r.wait.asking[k] = true
	}
	r.mu.Unlock()
	switch {
	case waiting:
		return org.Approval{}, refuse(conflict, "you're already waiting on approval #%d; one at a time.", a.ID)
	case asking:
		return org.Approval{}, refuse(conflict, "you're already asking for an approval; one at a time.")
	}
	defer func() {
		r.mu.Lock()
		delete(r.wait.asking, k)
		r.mu.Unlock()
	}()
	channel, asker := where, ""
	if x := r.outside.current(p.Nick); x != nil {
		asker = x.wake.Trigger.From
		if channel == "" {
			channel = x.wake.Channel
		}
	}
	if channel == "" && p.Team != "" {
		if t, err := r.store.Team(context.Background(), p.Team); err == nil {
			channel = t.Channel
		}
	}
	if channel != "" && !r.inChannel(p, channel) {
		return org.Approval{}, refuse(forbidden, "you're not in %s.", channel)
	}
	if asker == "" {
		asker = p.Nick
	}
	token, _ := json.Marshal(outsideResume{Outside: true, Tool: tool, Input: pinned})
	name := tool
	if name == "" {
		name = ownWords
	}
	a, ok := r.raiseApproval(p, channel, asker, &agent.ApprovalRequest{Tool: name, Summary: summary, Resume: string(token), Input: pinned})
	if !ok {
		return org.Approval{}, refuse(failed, "the approval couldn't be recorded; try again.")
	}
	return a, nil
}

// approvalNews is a decision, as the agent that asked is woken with it.
type approvalNews struct {
	ID       int64  `json:"id"`
	Summary  string `json:"summary"`
	Decision string `json:"decision"` // approved or denied
	By       string `json:"by"`
	Reason   string `json:"reason,omitempty"`
	Result   string `json:"result,omitempty"` // what GuildNet's tool did, once approved
}

// resumeOutside carries an outside agent on after a decision: GuildNet runs
// the approved tool, if there was one, and wakes the agent with the news.
func (r *Runner) resumeOutside(ctx context.Context, p config.Persona, d decision, onStep func(agent.Step)) (agent.Result, error) {
	a := d.approval
	var t outsideResume
	json.Unmarshal([]byte(a.Resume), &t)
	news := approvalNews{ID: a.ID, Summary: a.Summary, Decision: "denied", By: a.DecidedBy, Reason: d.reason}
	if news.By == "" {
		news.By = a.Approver
	}
	if d.allow {
		news.Decision = "approved"
		if tb := r.tools(); t.Tool != "" && tb != nil {
			if s, ok := tb.(interface {
				Step(name string, input map[string]any) string
			}); ok {
				if step := s.Step(t.Tool, t.Input); step != "" {
					onStep(agent.Step{Summary: step})
				}
			}
			out, err := tb.Run(ctx, p, t.Tool, t.Input)
			if err != nil {
				news.Result = "It failed: " + err.Error()
			} else {
				news.Result = out
				if line := agent.Headline(out); line != "" {
					onStep(agent.Step{Summary: line, Result: true})
				}
			}
		}
	}
	w := wakeUp{ID: "wk_" + randomID(), Type: "approval", Where: a.Channel, Channel: a.Channel, Transcript: []wakeLine{}, Approval: &news}
	if a.Channel == "" {
		w.Where, w.DMWith = "dm:"+a.Asker, a.Asker
	}
	w.Prompt.System = agent.SystemPrompt(r.cfg.Company, r.cfg.Owner, p, agent.Outside)
	w.Prompt.User = approvalPrompt(p, news)
	if deadline, ok := ctx.Deadline(); ok {
		w.ReplyBy = deadline.UTC()
	}
	answer, err := r.deliverWake(ctx, p, w, onStep)
	if err != nil {
		return agent.Result{}, fmt.Errorf("%s couldn't be told #%d was %s: %w", p.Nick, a.ID, news.Decision, err)
	}
	if answer.gaveUp != "" {
		return agent.Result{}, fmt.Errorf("its program outside GuildNet gave up: %s", answer.gaveUp)
	}
	return agent.Result{Reply: answer.reply}, nil
}

// approvalPrompt tells the agent the news, in words for any model.
func approvalPrompt(p config.Persona, n approvalNews) string {
	s := fmt.Sprintf("Your request #%d to %s was %s by %s.", n.ID, n.Summary, n.Decision, n.By)
	if n.Reason != "" {
		s += " Their reason: " + n.Reason
	}
	if n.Result != "" {
		s += "\nGuildNet did it: " + n.Result
	}
	return s + fmt.Sprintf("\nSay what happens next, briefly, as %s; or reply with exactly %s if nothing needs saying.", p.Nick, agent.Pass)
}

// The REST calls and the MCP server.

func (r *Runner) agentToolRoutes(mux *http.ServeMux) {
	persona := func(w http.ResponseWriter, req *http.Request) (config.Persona, bool) {
		p, err := r.store.Agent(req.Context(), agentOf(req))
		if err != nil {
			apiRefusal(w, r.storeRefusal(agentOf(req), err))
			return p, false
		}
		return p, true
	}
	cleanIn := func(w http.ResponseWriter, s string) (string, bool) { return cleanAgentText(w, s) }
	mux.HandleFunc("POST /agents/v1/messages", func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Where, Text string }
		if !decode(w, req, &body) {
			return
		}
		p, ok := persona(w, req)
		if !ok {
			return
		}
		text, ok := cleanIn(w, body.Text)
		if !ok {
			return
		}
		if err := r.agentPost(p, strings.TrimSpace(body.Where), text); err != nil {
			apiRefusal(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /agents/v1/context", func(w http.ResponseWriter, req *http.Request) {
		p, ok := persona(w, req)
		if !ok {
			return
		}
		lines, _ := strconv.Atoi(req.URL.Query().Get("lines"))
		got, err := r.agentContext(p, req.URL.Query().Get("where"), lines)
		if err != nil {
			apiRefusal(w, err)
			return
		}
		apiJSON(w, http.StatusOK, map[string]any{"where": req.URL.Query().Get("where"), "lines": got})
	})
	mux.HandleFunc("POST /agents/v1/approvals", func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Summary, Where string }
		if !decode(w, req, &body) {
			return
		}
		p, ok := persona(w, req)
		if !ok {
			return
		}
		summary, ok := cleanIn(w, body.Summary)
		if !ok {
			return
		}
		a, err := r.agentAsk(p, summary, strings.TrimSpace(body.Where), "", nil)
		if err != nil {
			apiRefusal(w, err)
			return
		}
		apiJSON(w, http.StatusAccepted, approvalForAgent(a))
	})
	mux.HandleFunc("GET /agents/v1/approvals/{id}", func(w http.ResponseWriter, req *http.Request) {
		id, _ := strconv.ParseInt(req.PathValue("id"), 10, 64)
		a, err := r.store.Approval(req.Context(), id)
		if err != nil || !strings.EqualFold(a.Agent, agentOf(req)) {
			apiError(w, http.StatusNotFound, "no approval of yours has that number")
			return
		}
		apiJSON(w, http.StatusOK, approvalForAgent(a))
	})
	mux.HandleFunc("GET /agents/v1/tools", func(w http.ResponseWriter, req *http.Request) {
		p, ok := persona(w, req)
		if !ok {
			return
		}
		out := []map[string]any{}
		for _, t := range r.agentTools(p) {
			out = append(out, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schemaOf(t),
				"needs_approval": t.Ask})
		}
		apiJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /agents/v1/tools/{name}", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input map[string]any `json:"input"`
			Where string         `json:"where"`
		}
		if !decode(w, req, &body) {
			return
		}
		p, ok := persona(w, req)
		if !ok {
			return
		}
		if body.Input == nil {
			body.Input = map[string]any{}
		}
		if body.Where != "" {
			body.Input["where"] = body.Where
		}
		if b, _ := json.Marshal(body.Input); len(b) > maxAgentText*16 {
			apiError(w, http.StatusRequestEntityTooLarge, "that input is too big")
			return
		}
		if msg := cleanInput(body.Input); msg != "" {
			apiError(w, http.StatusRequestEntityTooLarge, msg)
			return
		}
		res, err := r.agentUse(req.Context(), p, req.PathValue("name"), body.Input)
		if err != nil {
			apiRefusal(w, err)
			return
		}
		if res.Approval != nil {
			apiJSON(w, http.StatusAccepted, map[string]any{"approval": approvalForAgent(*res.Approval)})
			return
		}
		apiJSON(w, http.StatusOK, map[string]any{"output": res.Output})
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		p, err := r.store.Agent(req.Context(), agentOf(req))
		if err != nil {
			return nil
		}
		return r.mcpServer(p)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux.Handle("/agents/v1/mcp", mcpHandler)
}

// approvalForAgent is an approval as the agent that asked sees it.
func approvalForAgent(a org.Approval) map[string]any {
	out := map[string]any{"id": a.ID, "summary": a.Summary, "status": a.Status}
	if a.Status == "" {
		out["status"] = "pending"
	}
	if a.DecidedBy != "" {
		out["decided_by"] = a.DecidedBy
	}
	if a.Reason != "" {
		out["reason"] = a.Reason
	}
	return out
}

// schemaOf is a tool's input as JSON Schema.
func schemaOf(t agent.Tool) map[string]any {
	required := t.Required
	if required == nil {
		required = []string{}
	}
	props := t.Properties
	if props == nil {
		props = map[string]any{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// cleanInput takes control characters out of a tool's text inputs, as from
// an outside agent over MCP or the REST API alike, and says what's wrong
// if one is too long.
func cleanInput(in map[string]any) string {
	for k, v := range in {
		if s, ok := v.(string); ok {
			if len(s) > maxAgentText*16 {
				return k + " is too long"
			}
			in[k] = cleanText(s)
		}
	}
	return ""
}

// mcpServer is p's tools as an MCP server.
func (r *Runner) mcpServer(p config.Persona) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "guildnet", Title: "GuildNet", Version: "1"}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("You are %s on GuildNet. These tools act as you: post where you belong, read the conversation, ask for approval, and use your team's tools. Risky tools wait for a person; you're woken with the decision.", p.Nick),
	})
	for _, t := range r.agentTools(p) {
		name := t.Name
		s.AddTool(&mcp.Tool{Name: name, Description: t.Description, InputSchema: schemaOf(t),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t.ReadOnly}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				in := map[string]any{}
				if len(req.Params.Arguments) > 0 && json.Unmarshal(req.Params.Arguments, &in) != nil {
					return toolError("the arguments weren't a JSON object"), nil
				}
				if msg := cleanInput(in); msg != "" {
					return toolError(msg), nil
				}
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				res, err := r.agentUse(ctx, p, name, in)
				if err != nil {
					return toolError(err.Error()), nil
				}
				text := res.Output
				if res.Approval != nil {
					text = fmt.Sprintf("Waiting for approval #%d: %s. You'll be woken with the decision; don't wait for it.", res.Approval.ID, res.Approval.Summary)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
			})
	}
	return s
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}
