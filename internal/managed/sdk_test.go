// SPDX-License-Identifier: AGPL-3.0-only

package managed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/managed"
)

// fakeAPI is the Managed Agents API, enough for agent configs: it keeps the
// agents it's given and what each create or update sent.
type fakeAPI struct {
	mu     sync.Mutex
	agents []map[string]any
	sent   []map[string]any // agent create and update bodies, in order
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
		reply(map[string]any{"data": []any{}})
	case r.URL.Path == "/v1/environments" && r.Method == http.MethodPost:
		reply(map[string]any{"id": "env_1", "name": body["name"]})
	case r.URL.Path == "/v1/agents" && r.Method == http.MethodGet:
		reply(map[string]any{"data": f.agents})
	case r.URL.Path == "/v1/agents" && r.Method == http.MethodPost:
		f.sent = append(f.sent, body)
		a := map[string]any{"id": "agent_1", "version": 1, "metadata": body["metadata"]}
		f.agents = append(f.agents, a)
		reply(a)
	case strings.HasPrefix(r.URL.Path, "/v1/agents/") && r.Method == http.MethodPost:
		f.sent = append(f.sent, body)
		a := f.agents[0]
		a["version"] = a["version"].(int) + 1
		a["metadata"] = body["metadata"]
		reply(a)
	default:
		http.Error(w, "not faked: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// lastModel is the model config the last create or update sent.
func (f *fakeAPI) lastModel(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no agent was created or updated")
	}
	m, _ := f.sent[len(f.sent)-1]["model"].(map[string]any)
	return m
}

func sdkDesks(t *testing.T) (*managed.SDKDesks, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	d, err := managed.NewSDKDesks(context.Background(), managed.SDKOptions{Network: "acme", APIKey: "made-up"})
	if err != nil {
		t.Fatal(err)
	}
	return d, api
}

func TestAnAgentWorksAtItsEffort(t *testing.T) {
	d, api := sdkDesks(t)
	p := config.Persona{Nick: "rev", Name: "Rev", Model: "claude-sonnet-5-5", Effort: "high"}
	if _, _, err := d.EnsureAgent(context.Background(), p, "You review code.", nil); err != nil {
		t.Fatal(err)
	}
	if got := api.lastModel(t)["effort"]; got != "high" {
		t.Fatalf("effort sent = %v, want high", got)
	}
}

func TestChangingAnAgentsEffortChangesItsDesk(t *testing.T) {
	d, api := sdkDesks(t)
	p := config.Persona{Nick: "rev", Name: "Rev", Model: "claude-sonnet-5-5", Effort: "high"}
	if _, _, err := d.EnsureAgent(context.Background(), p, "You review code.", nil); err != nil {
		t.Fatal(err)
	}
	p.Effort = "low"
	if _, _, err := d.EnsureAgent(context.Background(), p, "You review code.", nil); err != nil {
		t.Fatal(err)
	}
	if got := api.lastModel(t)["effort"]; got != "low" {
		t.Fatalf("effort sent = %v, want low", got)
	}
}

func TestAModelWithoutEffortIsSentNone(t *testing.T) {
	d, api := sdkDesks(t)
	p := config.Persona{Nick: "triage", Name: "Triage", Model: "claude-haiku-4-5", Effort: "low"}
	if _, _, err := d.EnsureAgent(context.Background(), p, "You triage issues.", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := api.lastModel(t)["effort"]; ok {
		t.Fatalf("effort sent = %v, want none (Haiku 4.5 takes no effort setting)", got)
	}
}
