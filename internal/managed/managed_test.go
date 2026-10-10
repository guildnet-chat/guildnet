// SPDX-License-Identifier: AGPL-3.0-only

package managed_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/managed"
	"github.com/guildnet-chat/guildnet/internal/org"
)

// The Managed Agents adapter turns a desk session's events into what the
// runner understands: steps, a reply, an approval pause, and a cost.

// fakeDesks scripts what desk sessions do.
type fakeDesks struct {
	mu        sync.Mutex
	agents    map[string]int64  // nick -> agent version
	sessions  int               // sessions created so far
	sent      []string          // wake-up texts
	systems   []string          // system prompts the agent was given
	tools     [][]agent.Tool    // tools the agent was given, per EnsureAgent
	answers   [][]string        // per Answer call: "session/event allow|deny|result|error: text"
	script    [][]managed.Event // events for each successive send or confirm
	hold      chan struct{}     // if set, the desk stops after its first event until this is closed
	archived  []string
	lastAgent string
}

func (f *fakeDesks) EnsureAgent(_ context.Context, p config.Persona, system string, tools []agent.Tool) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.systems = append(f.systems, system)
	f.tools = append(f.tools, tools)
	if f.agents == nil {
		f.agents = map[string]int64{}
	}
	key := p.Nick + "|" + p.Model + "|" + system
	if f.lastAgent != key {
		f.agents[p.Nick]++
		f.lastAgent = key
	}
	return "agent_" + p.Nick, f.agents[p.Nick], nil
}

func (f *fakeDesks) NewDesk(_ context.Context, p config.Persona, agentID string, version int64) (string, error) {
	nick := p.Nick
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
	return "sesn_" + nick + "_" + string(rune('0'+f.sessions)), nil
}

func (f *fakeDesks) next() []managed.Event {
	if len(f.script) == 0 {
		return []managed.Event{{Kind: managed.Idle, Stop: "end_turn"}}
	}
	evs := f.script[0]
	f.script = f.script[1:]
	return evs
}

func (f *fakeDesks) Send(_ context.Context, _ config.Persona, session, text string, live func(managed.Event)) ([]managed.Event, error) {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	evs, hold := f.next(), f.hold
	f.mu.Unlock()
	return f.play(evs, hold, live), nil
}

// play delivers events as they happen, like a desk's stream.
func (f *fakeDesks) play(evs []managed.Event, hold chan struct{}, live func(managed.Event)) []managed.Event {
	for i, e := range evs {
		live(e)
		if i == 0 && hold != nil {
			<-hold
		}
	}
	return evs
}

func (f *fakeDesks) Answer(_ context.Context, _ config.Persona, session string, answers []managed.Answer, live func(managed.Event)) ([]managed.Event, error) {
	f.mu.Lock()
	var got []string
	for _, a := range answers {
		kind := map[bool]string{true: "allow", false: "deny"}[a.Allow]
		if a.Result {
			kind = map[bool]string{true: "error", false: "result"}[a.IsError]
		}
		got = append(got, session+"/"+a.EventID+" "+kind+": "+a.Text)
	}
	f.answers = append(f.answers, got)
	evs, hold := f.next(), f.hold
	f.mu.Unlock()
	return f.play(evs, hold, live), nil
}

// allAnswers flattens what was sent back to desks.
func (f *fakeDesks) allAnswers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.answers {
		out = append(out, a...)
	}
	return out
}

func (f *fakeDesks) Archive(_ context.Context, agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived = append(f.archived, agentID)
	return nil
}

var kai = config.Persona{Nick: "kai", Name: "Kai", Role: "Backend engineer", Model: "claude-sonnet-5",
	ReportsTo: "vera", Platform: "managed"}

func wake(text string) agent.Request {
	line := chat.Line{Nick: "ada", Text: text}
	return agent.Request{Persona: kai, Where: "#engineering", Trigger: line, Transcript: []chat.Line{line},
		Team: "- ada (human): Founder, owner\n"}
}

func newBrain(t *testing.T, desks *fakeDesks) *managed.Brain {
	t.Helper()
	return managed.New(desks, org.NewMemory(org.Seed{}), managed.Options{Company: "GuildNet builds GuildNet.", Owner: "ada"})
}

func newBrainWithTools(t *testing.T, desks *fakeDesks, tools *fakeToolbox) *managed.Brain {
	t.Helper()
	return managed.New(desks, org.NewMemory(org.Seed{}), managed.Options{Company: "GuildNet builds GuildNet.", Owner: "ada", Toolbox: tools})
}

func collect(steps *[]string) func(agent.Step) {
	return func(s agent.Step) { *steps = append(*steps, s.Summary) }
}

func TestToolUseShowsAsStepsAndTheFinalMessageIsTheReply(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{{
		{Kind: managed.Message, Text: "let me check the tests"},
		{Kind: managed.ToolUse, Tool: "bash", Input: map[string]any{"command": "go test ./..."}},
		{Kind: managed.ToolUse, Tool: "read", Input: map[string]any{"file_path": "/workspace/guildnet/README.md"}},
		{Kind: managed.Message, Text: "all green: 42 tests pass"},
		{Kind: managed.Idle, Stop: "end_turn"},
	}}}
	var steps []string

	res, err := newBrain(t, desks).Respond(context.Background(), wake("kai: run the tests"), collect(&steps))

	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "all green: 42 tests pass" {
		t.Errorf("reply = %q, want the final message", res.Reply)
	}
	want := []string{"runs go test ./...", "reads guildnet/README.md"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", steps, want)
	}
}

func TestTheAgentGetsItsToolsAndTheIRCContext(t *testing.T) {
	desks := &fakeDesks{}

	newBrain(t, desks).Respond(context.Background(), wake("kai: what's up?"), func(agent.Step) {})

	if len(desks.systems) == 0 || !strings.Contains(desks.systems[0], "workspace") {
		t.Errorf("the agent should be told it has a workspace and tools; system prompt: %q", desks.systems)
	}
	if len(desks.sent) != 1 || !strings.Contains(desks.sent[0], "kai: what's up?") || !strings.Contains(desks.sent[0], "#engineering") {
		t.Errorf("the wake-up should carry the IRC context, got %q", desks.sent)
	}
}

func TestARiskyToolPausesForApproval(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{{
		{Kind: managed.Message, Text: "PR #17 is green, merging"},
		{Kind: managed.ToolUse, ID: "sevt_9", MCPServer: "github", Tool: "merge_pull_request",
			Input: map[string]any{"pullNumber": 17.0, "repo": "guildnet"}, Ask: true},
		{Kind: managed.Idle, Stop: "requires_action", Waiting: []string{"sevt_9"}},
	}}}

	res, err := newBrain(t, desks).Respond(context.Background(), wake("kai: ship it"), func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if res.Approval == nil {
		t.Fatal("expected a pause for approval")
	}
	if res.Approval.Tool != "github.merge_pull_request" || !strings.Contains(res.Approval.Summary, "merge_pull_request") {
		t.Errorf("approval = %+v", res.Approval)
	}
	if res.Reply != "PR #17 is green, merging" {
		t.Errorf("what it said before pausing should still be posted, got %q", res.Reply)
	}
}

func TestADecisionIsSentBackAndTheAgentCarriesOn(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{
			{Kind: managed.ToolUse, ID: "sevt_9", MCPServer: "github", Tool: "merge_pull_request", Ask: true},
			{Kind: managed.Idle, Stop: "requires_action", Waiting: []string{"sevt_9"}},
		},
		{
			{Kind: managed.Message, Text: "ok, left it open for after launch"},
			{Kind: managed.Idle, Stop: "end_turn"},
		},
	}}
	b := newBrain(t, desks)
	res, _ := b.Respond(context.Background(), wake("kai: ship it"), func(agent.Step) {})

	after, err := b.Resume(context.Background(), kai, res.Approval.Resume, false, "not before launch", func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.HasSuffix(got[0], "sevt_9 deny: not before launch") {
		t.Errorf("confirmations sent: %q", got)
	}
	if after.Reply != "ok, left it open for after launch" {
		t.Errorf("reply after the decision = %q", after.Reply)
	}
}

func TestCostIsWhatTheDeskSpentSinceLastTime(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{{Kind: managed.Usage, CostCents: 3}, {Kind: managed.Message, Text: "one"}, {Kind: managed.Idle, Stop: "end_turn"}},
		{{Kind: managed.Usage, CostCents: 5}, {Kind: managed.Message, Text: "two"}, {Kind: managed.Idle, Stop: "end_turn"}},
	}}
	b := newBrain(t, desks)

	first, _ := b.Respond(context.Background(), wake("kai: one"), func(agent.Step) {})
	second, _ := b.Respond(context.Background(), wake("kai: two"), func(agent.Step) {})

	if first.Cost != 0.03 || second.Cost != 0.02 {
		t.Errorf("costs = %v then %v, want 0.03 then 0.02 (cumulative 3c then 5c)", first.Cost, second.Cost)
	}
}

func TestADeskAtItsBudgetSaysSo(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{{{Kind: managed.Idle, Stop: "budget_reached"}}}}

	_, err := newBrain(t, desks).Respond(context.Background(), wake("kai: more work"), func(agent.Step) {})

	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Errorf("err = %v, want one about the desk's budget", err)
	}
}

func TestAnAgentKeepsItsDeskBetweenWakeUps(t *testing.T) {
	desks := &fakeDesks{}
	b := newBrain(t, desks)

	b.Respond(context.Background(), wake("kai: one"), func(agent.Step) {})
	b.Respond(context.Background(), wake("kai: two"), func(agent.Step) {})

	if desks.sessions != 1 {
		t.Errorf("created %d desks for two wake-ups, want 1", desks.sessions)
	}
}

func TestAChangedProfileGetsANewDesk(t *testing.T) {
	desks := &fakeDesks{}
	b := newBrain(t, desks)
	b.Respond(context.Background(), wake("kai: one"), func(agent.Step) {})

	changed := wake("kai: two")
	changed.Persona.Model = "claude-opus-5"
	b.Respond(context.Background(), changed, func(agent.Step) {})

	if desks.sessions != 2 {
		t.Errorf("created %d desks, want a new one after the profile changed", desks.sessions)
	}
}

func TestDesksSurviveARestart(t *testing.T) {
	desks := &fakeDesks{}
	store := org.NewMemory(org.Seed{})
	opts := managed.Options{Company: "x", Owner: "ada"}
	managed.New(desks, store, opts).Respond(context.Background(), wake("kai: one"), func(agent.Step) {})

	managed.New(desks, store, opts).Respond(context.Background(), wake("kai: two"), func(agent.Step) {})

	if desks.sessions != 1 {
		t.Errorf("created %d desks across a restart, want the same desk reused", desks.sessions)
	}
}

func TestFiringArchivesTheAgent(t *testing.T) {
	desks := &fakeDesks{}
	b := newBrain(t, desks)
	b.Hire(context.Background(), kai)

	if err := b.Fire(context.Background(), kai); err != nil {
		t.Fatal(err)
	}
	if len(desks.archived) != 1 || desks.archived[0] != "agent_kai" {
		t.Errorf("archived %v, want agent_kai", desks.archived)
	}
}

func TestStepsShowWhileTheDeskIsStillWorking(t *testing.T) {
	hold := make(chan struct{})
	desks := &fakeDesks{hold: hold, script: [][]managed.Event{{
		{Kind: managed.ToolUse, Tool: "bash", Input: map[string]any{"command": "git log --oneline"}},
		{Kind: managed.Message, Text: "done"},
		{Kind: managed.Idle, Stop: "end_turn"},
	}}}
	steps := make(chan string, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		newBrain(t, desks).Respond(context.Background(), wake("kai: look at the history"), func(s agent.Step) { steps <- s.Summary })
	}()

	select {
	case s := <-steps:
		if s != "runs git log --oneline" {
			t.Errorf("step = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Error("the step should show while the desk is still working, not when it finishes")
	}
	close(hold)
	<-done
	if len(steps) != 0 {
		t.Errorf("steps shown twice: %q", <-steps)
	}
}

func TestADeskThatUsedItsBudgetIsReplacedOnTheNextWakeUp(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{{Kind: managed.Message, Text: "PR is up"}, {Kind: managed.Idle, Stop: "budget_reached"}},
		says("fresh desk, ready"),
	}}
	b := newBrain(t, desks)

	first, err := b.Respond(context.Background(), wake("kai: one"), func(agent.Step) {})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want the budget reported", err)
	}
	if first.Reply != "PR is up" {
		t.Errorf("what the agent said before running out = %q, want it kept", first.Reply)
	}
	second, err := b.Respond(context.Background(), wake("kai: two"), func(agent.Step) {})

	if err != nil || second.Reply != "fresh desk, ready" {
		t.Errorf("second wake-up: %q, %v", second.Reply, err)
	}
	if desks.sessions != 2 {
		t.Errorf("%d desks, want a new one after the budget ran out", desks.sessions)
	}
}
