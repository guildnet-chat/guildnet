// SPDX-License-Identifier: AGPL-3.0-only

package desk_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/brain"
	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/models"
)

// End to end, when GUILDNET_DOCKER_TESTS=1: an agent on a non-Claude model
// works at a desk of GuildNet's own. The runner's loop calls the model (a
// scripted stand-in for Gemini), which uses the workspace tools; they go
// over HTTP to sandboxd, which runs them in the agent's container in real
// Docker. A small image stands in for the desk image.

const e2eImage = "guildnet-desk-e2e:latest"

// scripted is a model provider that answers each call with the next of its
// answers, and keeps what it was sent.
type scripted struct {
	mu       sync.Mutex
	answers  []string
	requests []map[string]any
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(b, &req)
	s.mu.Lock()
	s.requests = append(s.requests, req)
	answer := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"out of script"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	if len(s.answers) > 0 {
		answer, s.answers = s.answers[0], s.answers[1:]
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, answer)
}

// uses is an answer asking for one tool.
func uses(id, name string, input map[string]any) string {
	args, _ := json.Marshal(input)
	quoted, _ := json.Marshal(string(args))
	return `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"` +
		id + `","type":"function","function":{"name":"` + name + `","arguments":` + string(quoted) + `}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

func says(text string) string {
	quoted, _ := json.Marshal(text)
	return `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":` +
		string(quoted) + `}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

// toolResults is what the model was told its tools gave, by call id.
func (s *scripted) toolResults() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	last := s.requests[len(s.requests)-1]
	for _, m := range last["messages"].([]any) {
		msg := m.(map[string]any)
		if msg["role"] == "tool" {
			out[msg["tool_call_id"].(string)] = msg["content"].(string)
		}
	}
	return out
}

func TestAnAgentOnAnyModelWorksAtADeskInRealDocker(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	build := exec.Command("docker", "build", "--quiet", "--tag", e2eImage, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\nRUN apk add --no-cache bash ripgrep coreutils git && adduser -u 1000 -D -h /home/desk desk && mkdir -p /workspace && chown desk:desk /workspace\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the image: %v: %s", err, out)
	}
	exec.Command("docker", "network", "create", "--internal", "guildnet-desks-test").Run()
	public, private := sandbox.NewKeys()
	backend := &sandbox.Docker{Image: e2eImage, Network: "guildnet-desks-test", Memory: "256m", CPUs: "1", Pids: 128}
	sandboxd := httptest.NewServer(sandbox.NewServer(sandbox.Options{Backend: backend, PublicKeys: []ed25519.PublicKey{public}}))
	defer sandboxd.Close()
	ws := desk.New(desk.NewRemote(sandboxd.URL, sandbox.TokenFor(private, "e2e", sandbox.Desks, time.Now(), 15*time.Minute)), nil)
	p := config.Persona{Nick: "gem", Name: "Gem", Role: "Engineer", Model: "google:gemini-3.5-flash-lite", ReportsTo: "method"}
	defer ws.Clear(context.Background(), p)
	ws.Clear(context.Background(), p)

	model := &scripted{answers: []string{
		uses("c1", "write", map[string]any{"path": "notes/plan.md", "content": "# Plan\nfix the flaky test\n"}),
		uses("c2", "edit", map[string]any{"path": "notes/plan.md", "old": "flaky", "new": "slow"}),
		uses("c3", "bash", map[string]any{"command": "cd notes && wc -l plan.md && id -un"}),
		uses("c4", "grep", map[string]any{"pattern": "slow test"}),
		uses("c5", "read", map[string]any{"path": "notes/plan.md"}),
		uses("c6", "bash", map[string]any{"command": "exit 7"}),
		says("Plan written: fix the slow test."),
	}}
	provider := httptest.NewServer(model)
	defer provider.Close()
	b := brain.New(brain.Options{Company: "Acme", Owner: "method",
		Keys: map[models.Provider]string{models.Google: "test-key"}, BaseURLs: map[models.Provider]string{models.Google: provider.URL}})
	desks := b.Desks(brain.DeskOptions{Toolbox: ws, Ready: ws.Ready, Retire: ws.Clear})
	var steps []string
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := desks.Respond(ctx, agent.Request{Persona: p, Where: "#engineering", Location: time.UTC,
		Trigger: chat.Line{Nick: "method", Text: "gem: write down the plan"}}, func(s agent.Step) { steps = append(steps, s.Summary) })

	if err != nil || res.Reply != "Plan written: fix the slow test." {
		t.Fatalf("reply %q, %v", res.Reply, err)
	}
	got := model.toolResults()
	for id, want := range map[string]string{
		"c1": "Wrote /workspace/notes/plan.md",
		"c2": "Edited /workspace/notes/plan.md",
		"c3": "2 plan.md\ndesk",
		"c4": "notes/plan.md:2:fix the slow test",
		"c5": "     2\tfix the slow test",
		"c6": "[exit status 7]",
	} {
		if !strings.Contains(got[id], want) {
			t.Errorf("%s gave %q, want %q in it", id, got[id], want)
		}
	}
	if joined := strings.Join(steps, " | "); !strings.Contains(joined, "writes notes/plan.md") || !strings.Contains(joined, "runs cd notes && wc -l plan.md") {
		t.Errorf("steps: %s", joined)
	}
}
