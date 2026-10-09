// SPDX-License-Identifier: AGPL-3.0-only

package brain_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/brain"
	"github.com/guildnet-chat/guildnet/models"
)

// Desks of GuildNet's own run on any model: the runner's own loop, with a
// workspace's tools beside GuildNet's, a longer leash than chat, and the
// desk readied (its sandbox woken, its repos checked out) before it works.

// readying counts the times a desk was readied, and how many model calls
// had been made by then.
type readying struct {
	mu     sync.Mutex
	times  int
	before []int // model calls made before each readying
	calls  func() int
	err    error
	fired  []string
}

func (r *readying) ready(_ context.Context, p config.Persona) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.times++
	r.before = append(r.before, r.calls())
	return r.err
}

func (r *readying) retire(_ context.Context, p config.Persona) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fired = append(r.fired, p.Nick)
	return nil
}

func deskBrain(t *testing.T, fake *fakeProvider, url string, tb agent.Toolbox) (*brain.Desks, *readying) {
	t.Helper()
	r := &readying{calls: fake.calls}
	b := toolBrain(models.Google, url, &fakeToolbox{none: true})
	return b.Desks(brain.DeskOptions{Toolbox: tb, Ready: r.ready, Retire: r.retire}), r
}

func TestAnAgentAtADeskOfOurOwnWorksInItsWorkspaceOnAnyModel(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_read_file", `{"path":"README.md"}`)),
		completion("The build is green.", `{"prompt_tokens":200,"completion_tokens":5,"total_tokens":205}`))
	tb := &fakeToolbox{}
	desks, ready := deskBrain(t, fake, url, tb)

	res, err := desks.Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)

	if err != nil || res.Reply != "The build is green." || len(tb.runs()) != 1 {
		t.Fatalf("reply %q, ran %v: %v", res.Reply, tb.runs(), err)
	}
	if ready.times != 1 || ready.before[0] != 0 {
		t.Errorf("readied %d times, after %v model calls; want once, before the model is called", ready.times, ready.before)
	}
	system := messagesOf(fake.request(t, 0))[0]["content"].(string)
	if !strings.Contains(system, "workspace") || strings.Contains(system, "web search") || !strings.Contains(system, "Read with github_read_file") {
		t.Errorf("the agent should be told it has a workspace (and not web tools it doesn't have), with its tools' guide:\n%s", system)
	}
}

func TestADeskThatCantBeReadiedSaysWhyAndCallsNoModel(t *testing.T) {
	fake, url := newFakeProvider(t, completion("hi", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	desks, ready := deskBrain(t, fake, url, &fakeToolbox{})
	ready.err = errors.New("the sandbox is starting")

	_, err := desks.Respond(context.Background(), wake("google:gemini-3.8-flash", ""), nil)

	if err == nil || !strings.Contains(err.Error(), "the sandbox is starting") {
		t.Errorf("err = %v, want why the desk isn't ready", err)
	}
	if n := fake.calls(); n != 0 {
		t.Errorf("the model was called %d times for a desk that wasn't ready", n)
	}
}

// Desk work takes more steps than a reply in chat, so desks may use tools
// for longer before they're stopped.
func TestADeskHasALongerLeashThanChat(t *testing.T) {
	fake, url := newFakeProvider(t, toolCalls(call("call_1", "github_read_file", `{"path":"README.md"}`)))
	desks, _ := deskBrain(t, fake, url, &fakeToolbox{})

	_, err := desks.Respond(context.Background(), wake("google:gemini-3.5-flash-lite", ""), nil)

	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("err = %v, want it stopped in the end", err)
	}
	if n := fake.calls(); n < 40 || n > 51 {
		t.Errorf("the model was called %d times, want about 50", n)
	}
}

// A desk's sandbox may have gone to sleep while a person decided, so it's
// readied again before the agent carries on.
func TestADeskIsReadiedAgainAfterAnApproval(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	fake.queue(toolCalls(call("call_1", "github_merge_pull_request", `{"number":7}`)),
		completion("Merged #7.", `{"prompt_tokens":300,"completion_tokens":4,"total_tokens":304}`))
	tb := &fakeToolbox{}
	desks, ready := deskBrain(t, fake, url, tb)
	p := wake("google:gemini-3.8-flash", "")
	paused, err := desks.Respond(context.Background(), p, nil)
	if err != nil || paused.Approval == nil {
		t.Fatalf("result %+v, %v; want a pause for approval", paused, err)
	}

	done, err := desks.Resume(context.Background(), p.Persona, paused.Approval.Resume, true, "", nil)

	if err != nil || done.Reply != "Merged #7." || len(tb.runs()) != 1 {
		t.Fatalf("after approval: %q, ran %v: %v", done.Reply, tb.runs(), err)
	}
	if ready.times != 2 {
		t.Errorf("readied %d times, want again before carrying on", ready.times)
	}
}

func TestFiringAnAgentClearsItsDesk(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	desks, ready := deskBrain(t, fake, url, &fakeToolbox{})

	if err := desks.Fire(context.Background(), config.Persona{Nick: "kai"}); err != nil {
		t.Fatal(err)
	}

	if len(ready.fired) != 1 || ready.fired[0] != "kai" {
		t.Errorf("cleared %v, want kai's desk", ready.fired)
	}
}

// A long piece of work piles up tool output; the oldest is dropped from the
// conversation once it grows large, so the model isn't swamped (and the
// approval token that carries the conversation stays small). The latest
// stays whole.
func TestOldToolOutputIsDroppedAsAConversationGrows(t *testing.T) {
	fake, url := newFakeProvider(t, "")
	for i := 0; i < 8; i++ {
		fake.queue(toolCalls(call("call_1", "github_read_file", `{"path":"big.txt"}`)))
	}
	fake.queue(completion("Done.", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}`))
	tb := &fakeToolbox{output: strings.Repeat("x", 60<<10)}
	desks, _ := deskBrain(t, fake, url, tb)

	res, err := desks.Respond(context.Background(), wake("google:gemini-3.5-flash-lite", ""), nil)

	if err != nil || res.Reply != "Done." {
		t.Fatalf("reply %q, %v", res.Reply, err)
	}
	msgs := messagesOf(fake.request(t, 8))
	var tools []string
	total := 0
	for _, m := range msgs {
		if m["role"] == "tool" {
			c := m["content"].(string)
			tools = append(tools, c)
			total += len(c)
		}
	}
	if len(tools) != 8 {
		t.Fatalf("%d tool results, want all 8 calls answered", len(tools))
	}
	if !strings.Contains(tools[0], "removed") {
		t.Errorf("the oldest output should be dropped, not %d bytes of it", len(tools[0]))
	}
	if len(tools[7]) != 60<<10 {
		t.Errorf("the latest output is %d bytes, want it whole", len(tools[7]))
	}
	if total > 300<<10 {
		t.Errorf("the model got %d KB of tool output", total>>10)
	}
}
