// SPDX-License-Identifier: AGPL-3.0-only

package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/brain"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
)

// Agents in chat, on any model, use GuildNet's tools (GitHub today): the
// runner runs the loop itself, calling the model with the tools, running
// what it asks for, and sending back the results until it answers. Risky
// tools wait for a person, as at a desk, and the agent carries on after.

// fakeToolbox is a team's GitHub, reduced to reading a file and merging.
type fakeToolbox struct {
	mu     sync.Mutex
	ran    []string // "tool input-json"
	output string   // what reading gives, if set
	none   bool     // the agent's team has no repos
}

func (f *fakeToolbox) Tools(p config.Persona) []agent.Tool {
	if f.none {
		return nil
	}
	return []agent.Tool{
		{Name: "github_read_file", Description: "Read a file", ReadOnly: true,
			Properties: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}},
		{Name: "github_merge_pull_request", Description: "Merge a PR", Ask: true,
			Properties: map[string]any{"number": map[string]any{"type": "integer"}}, Required: []string{"number"}},
	}
}

func (f *fakeToolbox) Guide(config.Persona) string {
	return "<github>Read with github_read_file.</github>"
}

func (f *fakeToolbox) Prepare(_ context.Context, _ config.Persona, name string, in map[string]any) (string, map[string]any, error) {
	return "merge PR #7 into main", map[string]any{"number": in["number"], "head_sha": "abc123"}, nil
}

func (f *fakeToolbox) Run(_ context.Context, _ config.Persona, name string, in map[string]any) (string, error) {
	b, _ := json.Marshal(in)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, name+" "+string(b))
	if name == "github_merge_pull_request" {
		return "Merged PR #7 (commit abc123).", nil
	}
	if f.output != "" {
		return f.output, nil
	}
	return "README.md: the build is green", nil
}

func (f *fakeToolbox) Step(name string, in map[string]any) string {
	if name == "github_read_file" {
		return "reads " + in["path"].(string)
	}
	return "merges PR #7"
}

func (f *fakeToolbox) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

func toolBrain(p models.Provider, url string, tb agent.Toolbox) *brain.Brain {
	return brain.New(brain.Options{Company: "Acme", Owner: "method", Toolbox: tb,
		Keys: map[models.Provider]string{p: "key-" + string(p)}, BaseURLs: map[models.Provider]string{p: url}})
}

// toolCalls is an OpenAI-style answer asking for tools.
func toolCalls(calls ...string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"refusal":null,"tool_calls":[` +
		strings.Join(calls, ",") + `]}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`
}

func call(id, name, args string) string {
	return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + quote(args) + `}}`
}

type steps struct {
	mu  sync.Mutex
	all []agent.Step
}

func (s *steps) on(st agent.Step) { s.mu.Lock(); s.all = append(s.all, st); s.mu.Unlock() }

func (s *steps) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, st := range s.all {
		out = append(out, st.Summary)
	}
	return strings.Join(out, " | ")
}

func messagesOf(req map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range req["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestAnAgentOnAnyModelUsesItsTools(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_read_file", `{"path":"README.md"}`)),
		completion("The README says the build is green.", `{"prompt_tokens":200,"completion_tokens":12,"total_tokens":212}`))
	tb := &fakeToolbox{}
	var st steps

	res, err := toolBrain(models.Google, url, tb).Respond(context.Background(), wake("google:gemini-3.8-flash", ""), st.on)

	if err != nil || res.Reply != "The README says the build is green." {
		t.Fatalf("reply %q, %v", res.Reply, err)
	}
	if runs := tb.runs(); len(runs) != 1 || runs[0] != `github_read_file {"path":"README.md"}` {
		t.Errorf("ran %v", runs)
	}
	first := fake.request(t, 0)
	tools, _ := first["tools"].([]any)
	if len(tools) != 2 || !strings.Contains(messagesOf(first)[0]["content"].(string), "Read with github_read_file") {
		t.Errorf("the model should be offered the tools and told how to use them: %v", first["tools"])
	}
	second := messagesOf(fake.request(t, 1))
	if n := len(second); n != 4 || second[2]["role"] != "assistant" || second[3]["role"] != "tool" ||
		second[3]["tool_call_id"] != "call_1" || !strings.Contains(second[3]["content"].(string), "the build is green") {
		t.Errorf("the second call should carry the tool call and its result: %v", second)
	}
	if res.Usage != (ledger.Usage{Input: 300, Output: 22}) {
		t.Errorf("usage %+v, want both calls'", res.Usage)
	}
	if got := st.text(); got != "reads README.md" {
		t.Errorf("steps %q: a read is shown as it happens, but not what it read", got)
	}
}

// responses is an OpenAI Responses API answer with these output items.
func responses(output string, in, cached, out int) string {
	return fmt.Sprintf(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":%s,
		"usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens":%d,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":%d}}`,
		output, in, cached, out, in+out)
}

// OpenAI's reasoning models take tools only through its Responses API.
// Their reasoning comes back encrypted, and goes back with the conversation
// (nothing is kept at OpenAI), so they keep their train of thought between
// tool calls.
func TestAnAgentOnAnOpenAIModelUsesToolsThroughTheResponsesAPI(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(responses(`[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc-1"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"github_read_file","arguments":"{\"path\":\"README.md\"}","status":"completed"}]`, 100, 20, 10),
		responses(`[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Green, per the README.","annotations":[]}]}]`, 200, 100, 8))
	tb := &fakeToolbox{}

	res, err := toolBrain(models.OpenAI, url, tb).Respond(context.Background(), wake("openai:gpt-6-luna", "high"), nil)

	if err != nil || res.Reply != "Green, per the README." || len(tb.runs()) != 1 {
		t.Fatalf("reply %q, ran %v: %v", res.Reply, tb.runs(), err)
	}
	first := fake.request(t, 0)
	if fake.paths[0] != "/v1/responses" || jsonOf(first["reasoning"]) != `{"effort":"high"}` || first["store"] != false ||
		!strings.Contains(jsonOf(first["include"]), "reasoning.encrypted_content") || !strings.Contains(first["instructions"].(string), "You are Kai") {
		t.Errorf("asked %s with reasoning %v, store %v, include %v", fake.paths[0], first["reasoning"], first["store"], first["include"])
	}
	if tools := jsonOf(first["tools"]); !strings.Contains(tools, `"type":"function"`) || !strings.Contains(tools, `"name":"github_read_file"`) {
		t.Errorf("tools = %s", tools)
	}
	second := jsonOf(fake.request(t, 1)["input"])
	for _, want := range []string{`"encrypted_content":"enc-1"`, `"type":"function_call"`, `{"call_id":"call_1","output":"README.md: the build is green","type":"function_call_output"}`, "is the build green?"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second call's input should have %s: %s", want, second)
		}
	}
	if res.Usage != (ledger.Usage{Input: 180, CacheRead: 120, Output: 18}) {
		t.Errorf("usage %+v", res.Usage)
	}
}

// Claude chat agents use tools the same way, through Claude's own API.
func TestClaudeInChatUsesToolsToo(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Checking."},
		{"type":"tool_use","id":"toolu_1","name":"github_read_file","input":{"path":"README.md"}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":10}}`,
		`{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Green, says the README."}],
		"stop_reason":"end_turn","usage":{"input_tokens":150,"output_tokens":8}}`)
	tb := &fakeToolbox{}

	res, err := toolBrain(models.Anthropic, strings.TrimSuffix(url, "v1/"), tb).Respond(context.Background(), wake("claude-sonnet-5-5", ""), nil)

	if err != nil || res.Reply != "Green, says the README." || len(tb.runs()) != 1 {
		t.Fatalf("reply %q, ran %v: %v", res.Reply, tb.runs(), err)
	}
	tools, _ := fake.request(t, 0)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["input_schema"] == nil {
		t.Errorf("tools = %v", tools)
	}
	msgs := messagesOf(fake.request(t, 1))
	if len(msgs) != 3 || msgs[1]["role"] != "assistant" || msgs[2]["role"] != "user" ||
		!strings.Contains(jsonOf(msgs[2]), `"tool_use_id":"toolu_1"`) || !strings.Contains(jsonOf(msgs[1]), `"id":"toolu_1"`) {
		t.Errorf("messages = %v", msgs)
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// A risky tool pauses the agent for a person's approval, with what the
// person is approving; once approved, it runs exactly that, and the agent
// carries on from where it was, even from another runner.
func TestARiskyToolWaitsForApprovalThenTheAgentCarriesOn(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_merge_pull_request", `{"number":7}`)),
		completion("Merged #7.", `{"prompt_tokens":300,"completion_tokens":4,"total_tokens":304}`))
	tb := &fakeToolbox{}

	paused, err := toolBrain(models.Google, url, tb).Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)

	if err != nil || paused.Approval == nil || paused.Approval.Summary != "merge PR #7 into main" || paused.Approval.Tool != "github_merge_pull_request" {
		t.Fatalf("result %+v, %v; want a pause for approval", paused, err)
	}
	if len(tb.runs()) != 0 {
		t.Fatal("the merge ran before anyone approved it")
	}
	var st steps
	later := toolBrain(models.Google, url, tb) // a restarted runner
	done, err := later.Resume(context.Background(), wake("google:gemini-3.8-flash", "").Persona, paused.Approval.Resume, true, "", st.on)

	if err != nil || done.Reply != "Merged #7." {
		t.Fatalf("after approval: %q, %v", done.Reply, err)
	}
	if runs := tb.runs(); len(runs) != 1 || !strings.Contains(runs[0], `"head_sha":"abc123"`) {
		t.Errorf("ran %v; want the merge as it was approved", runs)
	}
	last := messagesOf(fake.request(t, 1))
	if tool := last[len(last)-1]; tool["tool_call_id"] != "call_1" || !strings.Contains(tool["content"].(string), "Merged PR #7") {
		t.Errorf("the model should hear the merge's result: %v", tool)
	}
	if !strings.Contains(st.text(), "merged PR #7") {
		t.Errorf("steps %q should show what the merge did", st.text())
	}
}

func TestADeniedToolIsntRunAndTheAgentHearsWhy(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_merge_pull_request", `{"number":7}`)),
		completion("Understood, I'll wait for review.", `{"prompt_tokens":300,"completion_tokens":8,"total_tokens":308}`))
	tb := &fakeToolbox{}
	b := toolBrain(models.Google, url, tb)
	paused, _ := b.Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)

	done, err := b.Resume(context.Background(), wake("google:gemini-3.8-flash", "").Persona, paused.Approval.Resume, false, "needs review first", func(agent.Step) {})

	if err != nil || done.Reply != "Understood, I'll wait for review." || len(tb.runs()) != 0 {
		t.Fatalf("reply %q, ran %v: %v", done.Reply, tb.runs(), err)
	}
	last := messagesOf(fake.request(t, 1))
	if c := last[len(last)-1]["content"].(string); !strings.Contains(c, "Not approved") || !strings.Contains(c, "needs review first") {
		t.Errorf("the model heard %q", c)
	}
}

// Gemini's thinking rides along with its tool calls (a thought signature)
// and must come back with them, or it refuses the next call.
func TestGeminisThoughtSignaturesGoBackWithItsToolCalls(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(`{"id":"call_1","type":"function","function":{"name":"github_read_file","arguments":"{\"path\":\"README.md\"}"},
		"extra_content":{"google":{"thought_signature":"sig-123"}}}`),
		completion("Green.", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}`))

	_, err := toolBrain(models.Google, url, &fakeToolbox{}).Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)

	msgs := messagesOf(fake.request(t, 1))
	if err != nil || !strings.Contains(jsonOf(msgs[2]), `"thought_signature":"sig-123"`) {
		t.Errorf("the assistant's turn went back as %s (%v)", jsonOf(msgs[2]), err)
	}
}

// An agent that never stops using tools is stopped, as at a desk.
func TestAnAgentThatKeepsUsingToolsIsStopped(t *testing.T) {
	fake, url := newFakeProvider(t, toolCalls(call("call_1", "github_read_file", `{"path":"README.md"}`)))

	_, err := toolBrain(models.Google, url, &fakeToolbox{}).Respond(context.Background(), wake("google:gemini-3.5-flash-lite", ""), nil)

	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("err = %v, want it stopped", err)
	}
	if n := fake.calls(); n > 30 {
		t.Errorf("the model was called %d times", n)
	}
}

// countMeter lets a wake-up make so many model calls, and keeps what each
// one cost.
type countMeter struct {
	mu      sync.Mutex
	left    int
	charged []ledger.Usage
}

func (m *countMeter) Allow() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.left == 0 {
		return errors.New("kai's daily budget ($0.50) is used up")
	}
	m.left--
	return nil
}

func (m *countMeter) Charge(_ string, u ledger.Usage, _ float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charged = append(m.charged, u)
}

// A wake-up's budget is checked before each model call, and each call is
// charged as it comes back: a long wake-up stops when the money runs out,
// not at the end.
func TestAWakeUpStopsWhenItsBudgetRunsOut(t *testing.T) {
	fake, url := newFakeProvider(t, toolCalls(call("call_1", "github_read_file", `{"path":"README.md"}`)))
	m := &countMeter{left: 3}

	res, err := toolBrain(models.Google, url, &fakeToolbox{}).Respond(agent.WithMeter(context.Background(), m), wake("google:gemini-3.5-flash-lite", ""), nil)

	if err == nil || !strings.Contains(err.Error(), "used up") {
		t.Errorf("err = %v, want the budget's reason", err)
	}
	if n := fake.calls(); n != 3 {
		t.Errorf("the model was called %d times, want 3", n)
	}
	if len(m.charged) != 3 || m.charged[0] != (ledger.Usage{Input: 100, Output: 10}) {
		t.Errorf("charged %+v, want each of the 3 calls as it came back", m.charged)
	}
	if res.Usage != (ledger.Usage{Input: 300, Output: 30}) {
		t.Errorf("usage %+v, want the 3 calls'", res.Usage)
	}
}

// What a tool gives back is cut short past a limit, so one big read can't
// swamp the model (or the cost).
func TestAToolsOutputIsCappedForTheModel(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_read_file", `{"path":"big.txt"}`)),
		completion("Done.", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}`))
	tb := &fakeToolbox{output: strings.Repeat("x", 200<<10)}

	toolBrain(models.Google, url, tb).Respond(context.Background(), wake("google:gemini-3.5-flash-lite", ""), nil)

	msgs := messagesOf(fake.request(t, 1))
	if c := msgs[len(msgs)-1]["content"].(string); len(c) > 70<<10 || !strings.Contains(c, "cut short") {
		t.Errorf("the model got %d bytes of it", len(c))
	}
}

// A tool the agent wasn't given, or input that isn't JSON, goes back to the
// model as a failed call; nothing runs.
func TestABadToolCallIsToldToTheModel(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "rm_rf", `{}`), call("call_2", "github_read_file", `{not json`)),
		completion("Sorry.", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}`))
	tb := &fakeToolbox{}

	res, err := toolBrain(models.Google, url, tb).Respond(context.Background(), wake("google:gemini-3.5-flash-lite", ""), nil)

	msgs := messagesOf(fake.request(t, 1))
	if err != nil || res.Reply != "Sorry." || len(tb.runs()) != 0 || len(msgs) != 5 {
		t.Fatalf("reply %q, ran %v, %d messages: %v", res.Reply, tb.runs(), len(msgs), err)
	}
	if !strings.Contains(msgs[3]["content"].(string), "no tool called rm_rf") || !strings.Contains(msgs[4]["content"].(string), "JSON") {
		t.Errorf("results: %q, %q", msgs[3]["content"], msgs[4]["content"])
	}
}

// Without tools for its team (no repos), an agent is just called once, and
// told it has no tools.
func TestAnAgentWithNoToolsIsCalledOnce(t *testing.T) {
	fake, url := newFakeProvider(t, completion("Can't check, sorry.", `{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}`))

	res, err := toolBrain(models.OpenAI, url, &fakeToolbox{none: true}).Respond(context.Background(), wake("openai:gpt-6-luna", "low"), nil)

	req := fake.request(t, 0)
	if err != nil || res.Reply != "Can't check, sorry." || req["tools"] != nil || fake.calls() != 1 ||
		!strings.Contains(messagesOf(req)[0]["content"].(string), "you have no tools") {
		t.Errorf("reply %q, tools %v, calls %d: %v", res.Reply, req["tools"], fake.calls(), err)
	}
}

// asking is a toolbox that says, this time, a call to a tool that usually
// runs must ask first (as connectors do for a write to one service after
// reading another).
type asking struct{ *fakeToolbox }

func (asking) MustAsk(_ config.Persona, name string, _ map[string]any) (bool, string) {
	return name == "github_read_file", "it read from elsewhere just now"
}

func TestACallTheToolboxSaysMustAskWaitsForAPerson(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_read_file", `{"path":"README.md"}`)))
	tb := asking{&fakeToolbox{}}

	res, err := toolBrain(models.Google, url, tb).Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)

	if err != nil || res.Approval == nil {
		t.Fatalf("result %+v, %v; want a pause for approval", res, err)
	}
	if runs := tb.runs(); len(runs) != 0 {
		t.Errorf("ran %v before anyone approved", runs)
	}
}
