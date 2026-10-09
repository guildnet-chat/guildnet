// SPDX-License-Identifier: AGPL-3.0-only

package managed_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/managed"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
)

// Tools the runner carries out for an agent at its desk (GitHub today): the
// desk asks, the runner runs the tool with credentials the agent never sees,
// and the result goes back. Tools that need approval pause for a person.

type fakeToolbox struct {
	mu       sync.Mutex
	ran      []string          // "tool key=value ..." for each call run
	fail     map[string]string // tool -> error it fails with
	prepFail string            // Prepare fails with this, if set
}

// Agents in the Product team get no tools, as a team without integrations.
func (f *fakeToolbox) Tools(p config.Persona) []agent.Tool {
	if p.Team == "Product" {
		return nil
	}
	return []agent.Tool{
		{Name: "github_open_pull_request", Description: "Open a PR", Properties: map[string]any{"title": map[string]any{"type": "string"}}},
		{Name: "github_merge_pull_request", Description: "Merge a PR", Properties: map[string]any{"number": map[string]any{"type": "integer"}}, Ask: true},
	}
}

func (f *fakeToolbox) Guide(p config.Persona) string {
	if p.Team == "Product" {
		return ""
	}
	return "<github>Change code with github_open_pull_request.</github>"
}

func (f *fakeToolbox) Prepare(_ context.Context, _ config.Persona, name string, in map[string]any) (string, map[string]any, error) {
	if f.prepFail != "" {
		return "", nil, errors.New(f.prepFail)
	}
	pinned := map[string]any{"head_sha": "abc123"}
	for k, v := range in {
		pinned[k] = v
	}
	return fmt.Sprintf("merge PR #%v into main", in["number"]), pinned, nil
}

func (f *fakeToolbox) Run(_ context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kv []string
	for k, v := range in {
		kv = append(kv, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(kv)
	f.ran = append(f.ran, strings.TrimSpace(name+" "+strings.Join(kv, " ")))
	if msg, ok := f.fail[name]; ok {
		return "", errors.New(msg)
	}
	return "done: " + name + " for " + p.Nick, nil
}

func (f *fakeToolbox) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

func openPRCall(id string) managed.Event {
	return managed.Event{Kind: managed.ToolUse, ID: id, Custom: true, Tool: "github_open_pull_request",
		Input: map[string]any{"repo": "guildnet", "title": "Add healthz"}}
}

func mergeCall(id string) managed.Event {
	return managed.Event{Kind: managed.ToolUse, ID: id, Custom: true, Tool: "github_merge_pull_request",
		Input: map[string]any{"repo": "guildnet", "number": 17.0}}
}

func waitingOn(ids ...string) managed.Event {
	return managed.Event{Kind: managed.Idle, Stop: "requires_action", Waiting: ids}
}

func says(text string) []managed.Event {
	return []managed.Event{{Kind: managed.Message, Text: text}, {Kind: managed.Idle, Stop: "end_turn"}}
}

func TestTheRunnerRunsTheAgentsToolAndSendsBackTheResult(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{openPRCall("e1"), waitingOn("e1")},
		says("PR #3 is up for review"),
	}}
	tools := &fakeToolbox{}
	var steps []string

	res, err := newBrainWithTools(t, desks, tools).Respond(context.Background(), wake("kai: fix the health check"), collect(&steps))

	if err != nil {
		t.Fatal(err)
	}
	if got := tools.calls(); len(got) != 1 || got[0] != "github_open_pull_request repo=guildnet title=Add healthz" {
		t.Errorf("tool calls run = %q", got)
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.HasSuffix(got[0], "e1 result: done: github_open_pull_request for kai") {
		t.Errorf("answers = %q, want the tool's result sent back", got)
	}
	if res.Reply != "PR #3 is up for review" {
		t.Errorf("reply = %q, want what the agent said after the tool ran", res.Reply)
	}
	if len(steps) != 2 || !strings.Contains(steps[0], "pull request") || steps[1] != "done: github_open_pull_request for kai" {
		t.Errorf("steps = %q, want the tool call shown as a step, then its result", steps)
	}
}

// roundsMeter lets a wake-up carry on so many times, and adds up what it
// was charged.
type roundsMeter struct {
	mu      sync.Mutex
	left    int
	charged float64
}

func (m *roundsMeter) Allow() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.left == 0 {
		return errors.New("kai's daily budget ($1.00) is used up")
	}
	m.left--
	return nil
}

func (m *roundsMeter) Charge(_ string, _ ledger.Usage, cost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charged += cost
}

// A desk's spend is charged as it goes, and it isn't sent on again once the
// budget is used up.
func TestADeskStopsWhenItsBudgetRunsOut(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{{Kind: managed.Usage, CostCents: 30}, openPRCall("e1"), waitingOn("e1")},
		{{Kind: managed.Usage, CostCents: 60}, openPRCall("e2"), waitingOn("e2")},
		says("all done"),
	}}
	tools := &fakeToolbox{}
	m := &roundsMeter{left: 1}

	res, err := newBrainWithTools(t, desks, tools).Respond(agent.WithMeter(context.Background(), m), wake("kai: open PRs"), func(agent.Step) {})

	if err == nil || !strings.Contains(err.Error(), "used up") {
		t.Errorf("err = %v, want the budget's reason", err)
	}
	if got := tools.calls(); len(got) != 1 {
		t.Errorf("tools run: %q, want only the first round's", got)
	}
	if got := desks.allAnswers(); len(got) != 1 {
		t.Errorf("answers sent: %q, want the desk sent on once", got)
	}
	if m.charged < 0.599 || m.charged > 0.601 || res.Cost < 0.599 || res.Cost > 0.601 {
		t.Errorf("charged $%v as it went, result $%v; want $0.60", m.charged, res.Cost)
	}
}

func TestAFailedToolIsReportedToTheAgentWhichCarriesOn(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{openPRCall("e1"), waitingOn("e1")},
		says("GitHub refused: I'll use a branch"),
	}}
	tools := &fakeToolbox{fail: map[string]string{"github_open_pull_request": "can't commit straight to main"}}

	res, err := newBrainWithTools(t, desks, tools).Respond(context.Background(), wake("kai: fix it"), func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.HasSuffix(got[0], "e1 error: can't commit straight to main") {
		t.Errorf("answers = %q, want the failure sent back as an error", got)
	}
	if res.Reply != "GitHub refused: I'll use a branch" {
		t.Errorf("reply = %q", res.Reply)
	}
}

func TestSeveralToolCallsAreAnsweredTogether(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{openPRCall("e1"), openPRCall("e2"), waitingOn("e1", "e2")},
		says("two PRs up"),
	}}

	newBrainWithTools(t, desks, &fakeToolbox{}).Respond(context.Background(), wake("kai: two fixes"), func(agent.Step) {})

	if len(desks.answers) != 1 || len(desks.answers[0]) != 2 {
		t.Errorf("answers = %q, want both results in one reply to the desk", desks.answers)
	}
}

func TestTheAgentKeepsWorkingThroughSeveralRoundsOfTools(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{openPRCall("e1"), waitingOn("e1")},
		{openPRCall("e2"), waitingOn("e2")},
		says("both done"),
	}}
	tools := &fakeToolbox{}

	res, _ := newBrainWithTools(t, desks, tools).Respond(context.Background(), wake("kai: go"), func(agent.Step) {})

	if len(tools.calls()) != 2 || res.Reply != "both done" {
		t.Errorf("ran %q and replied %q, want two rounds then the reply", tools.calls(), res.Reply)
	}
}

func TestAToolThatNeedsApprovalPausesWithWhatThePersonWillApprove(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{{Kind: managed.Message, Text: "checks are green, asking to merge"}, mergeCall("e9"), waitingOn("e9")},
	}}
	tools := &fakeToolbox{}

	res, err := newBrainWithTools(t, desks, tools).Respond(context.Background(), wake("kai: ship it"), func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if res.Approval == nil {
		t.Fatal("expected a pause for approval")
	}
	if res.Approval.Tool != "github_merge_pull_request" || res.Approval.Summary != "merge PR #17 into main" {
		t.Errorf("approval = %+v", res.Approval)
	}
	if len(tools.calls()) != 0 || len(desks.answers) != 0 {
		t.Errorf("nothing should run or be answered before approval; ran %q, answered %q", tools.calls(), desks.answers)
	}
	if res.Reply != "checks are green, asking to merge" {
		t.Errorf("reply = %q", res.Reply)
	}
}

func TestApprovingRunsExactlyWhatWasApproved(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{mergeCall("e9"), waitingOn("e9")},
		says("merged, thanks"),
	}}
	tools := &fakeToolbox{}
	store := org.NewMemory(org.Seed{})
	opts := managed.Options{Company: "x", Owner: "ada", Toolbox: tools}
	res, _ := managed.New(desks, store, opts).Respond(context.Background(), wake("kai: ship it"), func(agent.Step) {})

	// A restart between asking and deciding: the approval carries what's needed.
	after, err := managed.New(desks, store, opts).Resume(context.Background(), kai, res.Approval.Resume, true, "", func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if got := tools.calls(); len(got) != 1 || got[0] != "github_merge_pull_request head_sha=abc123 number=17 repo=guildnet" {
		t.Errorf("ran %q, want the merge with the approved commit pinned", got)
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.Contains(got[0], "e9 result: done: github_merge_pull_request") {
		t.Errorf("answers = %q", got)
	}
	if after.Reply != "merged, thanks" {
		t.Errorf("reply = %q", after.Reply)
	}
}

func TestDenyingTellsTheAgentWhyAndRunsNothing(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{mergeCall("e9"), waitingOn("e9")},
		says("ok, holding off"),
	}}
	tools := &fakeToolbox{}
	b := newBrainWithTools(t, desks, tools)
	res, _ := b.Respond(context.Background(), wake("kai: ship it"), func(agent.Step) {})

	after, err := b.Resume(context.Background(), kai, res.Approval.Resume, false, "not before the demo", func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if len(tools.calls()) != 0 {
		t.Errorf("ran %q after a denial", tools.calls())
	}
	got := desks.allAnswers()
	if len(got) != 1 || !strings.Contains(got[0], "e9 error:") || !strings.Contains(got[0], "not before the demo") {
		t.Errorf("answers = %q, want a refusal carrying the reason", got)
	}
	if after.Reply != "ok, holding off" {
		t.Errorf("reply = %q", after.Reply)
	}
}

func TestOtherToolsInTheSameTurnRunNowAndTheirResultsWaitForTheDecision(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{openPRCall("e1"), mergeCall("e9"), waitingOn("e1", "e9")},
		says("done"),
	}}
	tools := &fakeToolbox{}
	b := newBrainWithTools(t, desks, tools)
	res, _ := b.Respond(context.Background(), wake("kai: go"), func(agent.Step) {})
	if res.Approval == nil || len(tools.calls()) != 1 {
		t.Fatalf("want the PR opened and the merge waiting; ran %q, approval %+v", tools.calls(), res.Approval)
	}

	b.Resume(context.Background(), kai, res.Approval.Resume, true, "", func(agent.Step) {})

	if len(desks.answers) != 1 || len(desks.answers[0]) != 2 {
		t.Errorf("answers = %q, want both results sent together after the decision", desks.answers)
	}
}

func TestAProblemFoundBeforeAskingGoesBackToTheAgentWithoutBotheringAnyone(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{mergeCall("e9"), waitingOn("e9")},
		says("PR #17 is already merged, never mind"),
	}}
	tools := &fakeToolbox{prepFail: "PR #17 is already merged"}

	res, err := newBrainWithTools(t, desks, tools).Respond(context.Background(), wake("kai: ship it"), func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if res.Approval != nil {
		t.Errorf("nobody should be asked: %+v", res.Approval)
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.HasSuffix(got[0], "e9 error: PR #17 is already merged") {
		t.Errorf("answers = %q", got)
	}
}

func TestOnlyOneApprovalIsAskedForAtATime(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{mergeCall("e8"), mergeCall("e9"), waitingOn("e8", "e9")},
	}}

	b := newBrainWithTools(t, desks, &fakeToolbox{})
	res, _ := b.Respond(context.Background(), wake("kai: ship both"), func(agent.Step) {})

	if res.Approval == nil {
		t.Fatal("expected an approval for the first merge")
	}
	desks.script = [][]managed.Event{says("ok")}
	if _, err := b.Resume(context.Background(), kai, res.Approval.Resume, true, "", func(agent.Step) {}); err != nil {
		t.Fatal(err)
	}
	got := desks.allAnswers()
	if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), "e9 error:") || !strings.Contains(strings.Join(got, "|"), "at a time") {
		t.Errorf("answers = %q, want the second merge told to ask again once the first is decided", got)
	}
}

func TestAnUnknownToolGetsAnErrorRatherThanLeavingTheDeskStuck(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{{Kind: managed.ToolUse, ID: "e1", Custom: true, Tool: "launch_rockets"}, waitingOn("e1")},
		says("ok"),
	}}

	_, err := newBrain(t, desks).Respond(context.Background(), wake("kai: go"), func(agent.Step) {})

	if err != nil {
		t.Fatal(err)
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.Contains(got[0], "e1 error:") {
		t.Errorf("answers = %q", got)
	}
}

func TestTheAgentIsGivenTheToolsAndToldHowToUseThem(t *testing.T) {
	desks := &fakeDesks{}

	newBrainWithTools(t, desks, &fakeToolbox{}).Respond(context.Background(), wake("kai: hi"), func(agent.Step) {})

	if len(desks.tools) == 0 || len(desks.tools[0]) != 2 || desks.tools[0][1].Name != "github_merge_pull_request" {
		t.Errorf("tools given = %+v", desks.tools)
	}
	if !strings.Contains(desks.systems[0], "Change code with github_open_pull_request") {
		t.Errorf("the system prompt should include the toolbox's guide: %q", desks.systems[0])
	}
}

func TestARunawayToolLoopStops(t *testing.T) {
	var script [][]managed.Event
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("e%d", i)
		script = append(script, []managed.Event{openPRCall(id), waitingOn(id)})
	}
	desks := &fakeDesks{script: script}

	_, err := newBrainWithTools(t, desks, &fakeToolbox{}).Respond(context.Background(), wake("kai: go"), func(agent.Step) {})

	if err == nil || len(desks.answers) >= 200 {
		t.Errorf("err = %v after %d rounds, want it stopped with an error well before 200", err, len(desks.answers))
	}
}

func (f *fakeToolbox) Step(name string, in map[string]any) string {
	if name == "github_open_pull_request" {
		return fmt.Sprintf("opens a pull request on %v: %v", in["repo"], in["title"])
	}
	return ""
}

func TestAToolsResultIsShownSoPeopleSeeItEvenIfTheAgentStopsShort(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{
		{openPRCall("e1"), waitingOn("e1")},
		{{Kind: managed.Idle, Stop: "budget_reached"}},
	}}
	var steps []agent.Step

	newBrainWithTools(t, desks, &fakeToolbox{}).Respond(context.Background(), wake("kai: open a PR"), func(s agent.Step) { steps = append(steps, s) })

	var results []string
	for _, s := range steps {
		if s.Result {
			results = append(results, s.Summary)
		}
	}
	if len(results) != 1 || results[0] != "done: github_open_pull_request for kai" {
		t.Errorf("results shown = %q, want the tool's result", results)
	}
}

func TestAFailedToolIsNotAnnouncedAsAResult(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{{openPRCall("e1"), waitingOn("e1")}, says("hm")}}
	var steps []agent.Step
	tools := &fakeToolbox{fail: map[string]string{"github_open_pull_request": "nope"}}

	newBrainWithTools(t, desks, tools).Respond(context.Background(), wake("kai: go"), func(s agent.Step) { steps = append(steps, s) })

	for _, s := range steps {
		if s.Result {
			t.Errorf("a failure was shown as a result: %q", s.Summary)
		}
	}
}

func TestAnAgentGetsOnlyTheToolsItsTeamHas(t *testing.T) {
	desks := &fakeDesks{}
	req := wake("vera: hi")
	req.Persona = config.Persona{Nick: "vera", Name: "Vera", Model: "claude-opus-5", Team: "Product", Platform: "managed"}

	newBrainWithTools(t, desks, &fakeToolbox{}).Respond(context.Background(), req, func(agent.Step) {})

	if len(desks.tools) == 0 || len(desks.tools[0]) != 0 {
		t.Errorf("tools given to a Product agent = %+v, want none", desks.tools)
	}
	if strings.Contains(desks.systems[0], "<github>") {
		t.Error("a team without GitHub shouldn't be told about it")
	}
}

func TestAToolOutsideTheAgentsTeamIsRefused(t *testing.T) {
	desks := &fakeDesks{script: [][]managed.Event{{openPRCall("e1"), waitingOn("e1")}, says("ok")}}
	tools := &fakeToolbox{}
	req := wake("vera: open a PR")
	req.Persona = config.Persona{Nick: "vera", Name: "Vera", Model: "claude-opus-5", Team: "Product", Platform: "managed"}

	newBrainWithTools(t, desks, tools).Respond(context.Background(), req, func(agent.Step) {})

	if len(tools.calls()) != 0 {
		t.Errorf("ran %q for an agent whose team doesn't have the tool", tools.calls())
	}
	if got := desks.allAnswers(); len(got) != 1 || !strings.Contains(got[0], "e1 error:") {
		t.Errorf("answers = %q", got)
	}
}
