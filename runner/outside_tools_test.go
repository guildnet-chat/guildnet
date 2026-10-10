// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// An outside agent acts back through GuildNet's tools: posting where it
// belongs, reading the conversation there, asking for approval, and the
// network's own tools (GitHub), where a risky one waits for a person and
// GuildNet runs exactly what was approved. Over plain REST, and as an MCP
// server that agent frameworks load.

// fakeTools is a team's GitHub, reduced to reading a file and merging.
type fakeTools struct {
	mu  sync.Mutex
	ran []string
}

func (f *fakeTools) Tools(config.Persona) []agent.Tool {
	return []agent.Tool{
		{Name: "github_read_file", Description: "Read a file", ReadOnly: true,
			Properties: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}},
		{Name: "github_merge_pull_request", Description: "Merge a PR", Ask: true,
			Properties: map[string]any{"number": map[string]any{"type": "integer"}}, Required: []string{"number"}},
	}
}

func (f *fakeTools) Guide(config.Persona) string { return "" }

func (f *fakeTools) Prepare(_ context.Context, _ config.Persona, _ string, in map[string]any) (string, map[string]any, error) {
	return "merge PR #7 into main", map[string]any{"number": in["number"], "head_sha": "abc123"}, nil
}

func (f *fakeTools) Run(_ context.Context, _ config.Persona, name string, in map[string]any) (string, error) {
	b, _ := json.Marshal(in)
	f.mu.Lock()
	f.ran = append(f.ran, name+" "+string(b))
	f.mu.Unlock()
	if name == "github_merge_pull_request" {
		return "Merged PR #7 (commit abc123).", nil
	}
	return "README.md: the build is green", nil
}

func (f *fakeTools) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

func TestAnOutsideAgentPostsWhereItBelongsAndOnlyThere(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))

	status, _, _ := agent.call("POST", "/messages", map[string]any{"where": "#engineering", "text": "Release 1.4 is out."})

	if status != http.StatusNoContent {
		t.Fatalf("posting in #engineering: %d", status)
	}
	n.human.expect("kai's post", fromContaining("kai", "#engineering", "Release 1.4 is out."))
	if status, _, _ := agent.call("POST", "/messages", map[string]any{"where": "#marketing", "text": "hi"}); status != http.StatusForbidden {
		t.Errorf("posting in #marketing, which kai isn't in: %d", status)
	}
	if status, _, _ := agent.call("POST", "/messages", map[string]any{"where": "method", "text": "psst"}); status != http.StatusForbidden {
		t.Errorf("DMing someone out of the blue: %d", status)
	}
}

func TestAnOutsideAgentReadsTheConversationWhereItsAMember(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	n.human.say("#engineering", "the staging deploy is flaky again")

	var status int
	var out map[string]any
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if status, out, _ = agent.call("GET", "/context?where=%23engineering&lines=20", nil); strings.Contains(jsonText(out), "flaky again") {
			break
		}
	}

	if status != http.StatusOK || !strings.Contains(jsonText(out), "the staging deploy is flaky again") {
		t.Errorf("#engineering's conversation: %d %v", status, out)
	}
	if status, _, _ := agent.call("GET", "/context?where=%23marketing", nil); status != http.StatusForbidden {
		t.Errorf("#marketing's: %d", status)
	}
}

// Asking for approval: the right person is asked, the agent gets a handle
// at once, and it's woken with the decision.
func TestAnOutsideAgentAsksForApprovalAndIsWokenWithTheDecision(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	agent.inbox(0)

	status, out, _ := agent.call("POST", "/approvals", map[string]any{"summary": "deploy 1.4 to production", "where": "#engineering"})

	if status != http.StatusAccepted || out["status"] != "pending" || out["id"] == nil {
		t.Fatalf("asking: %d %v", status, out)
	}
	n.human.expect("method asked to decide", guildServSaying(ownerAccount, "deploy 1.4 to production"))
	n.human.say("GuildServ", "APPROVE 1")

	wakes := agent.inbox(5)
	if len(wakes) != 1 || wakes[0]["type"] != "approval" {
		t.Fatalf("woken with %v, want the decision", wakes)
	}
	a, _ := wakes[0]["approval"].(map[string]any)
	if a["decision"] != "approved" || a["by"] != ownerAccount || a["summary"] != "deploy 1.4 to production" {
		t.Errorf("the decision: %v", a)
	}
	agent.call("POST", "/wakes/"+wakes[0]["id"].(string)+"/reply", map[string]any{"text": "Deploying 1.4 now."})
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "Deploying 1.4 now."))
	if _, out, _ := agent.call("GET", "/approvals/1", nil); out["status"] != "approved" {
		t.Errorf("approval #1: %v", out)
	}
}

// What an outside agent asks to do in its own words is said to be its own:
// nothing pins it to an action, so the approver mustn't take it for a
// description GuildNet checked. A tool's approval is described by the tool,
// from the input that will run.
func TestAnOutsideAgentsOwnWordsAreMarkedAsTheirs(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	agent.inbox(0)

	agent.call("POST", "/approvals", map[string]any{"summary": "merge PR #41 (a typo)", "where": "#engineering"})

	n.human.expect("method asked, the words marked as kai's", guildServSaying(ownerAccount, `kai asks, in its own words, to “merge PR #41 (a typo)”`))
	n.human.expect("the ledger saying so in #engineering", fromContaining("ledger", "#engineering", `kai asks, in its own words, to “merge PR #41 (a typo)”`))
}

// Pausing stops outside agents too: they can't post or use tools until the
// network's agents are resumed.
func TestAPausedOutsideAgentCantPostOrUseTools(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	n.runner.UseTools(&fakeTools{})
	agent := n.outsider(outside(t, n))
	agent.inbox(0)
	n.human.say("#engineering", "!pause")
	n.human.expect("paused", fromContaining("ledger", "#engineering", "paused"))

	if status, _, _ := agent.call("POST", "/messages", map[string]any{"where": "#engineering", "text": "still here"}); status != http.StatusConflict {
		t.Errorf("posting while paused: %d, want 409", status)
	}
	if status, _, _ := agent.call("POST", "/tools/github_read_file", map[string]any{"input": map[string]any{"path": "README.md"}}); status != http.StatusConflict {
		t.Errorf("a tool while paused: %d, want 409", status)
	}

	n.human.say("#engineering", "!resume")
	n.human.expect("resumed", fromContaining("ledger", "#engineering", "resumed"))
	if status, _, _ := agent.call("POST", "/messages", map[string]any{"where": "#engineering", "text": "back"}); status != http.StatusNoContent {
		t.Errorf("posting once resumed: %d", status)
	}
}

// The network's own tools: a read runs at once; a merge waits for a person,
// and GuildNet merges exactly what was approved, then wakes the agent with
// the result.
func TestAnOutsideAgentUsesTheNetworksToolsAndRiskyOnesWait(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	tools := &fakeTools{}
	n.runner.UseTools(tools)
	agent := n.outsider(outside(t, n))
	agent.inbox(0)

	_, _, list := agent.call("GET", "/tools", nil)
	if names := jsonText(list); !strings.Contains(names, "github_read_file") || !strings.Contains(names, "post_message") || !strings.Contains(names, `"required":["path"]`) {
		t.Errorf("tools: %s", names)
	}
	if status, out, _ := agent.call("POST", "/tools/github_read_file", map[string]any{"input": map[string]any{"path": "README.md"}}); status != http.StatusOK || !strings.Contains(out["output"].(string), "green") {
		t.Errorf("reading: %d %v", status, out)
	}

	status, out, _ := agent.call("POST", "/tools/github_merge_pull_request", map[string]any{"input": map[string]any{"number": 7}, "where": "#engineering"})
	if pending, _ := out["approval"].(map[string]any); status != http.StatusAccepted || pending["status"] != "pending" {
		t.Fatalf("merging: %d %v", status, out)
	}
	if len(tools.runs()) != 1 {
		t.Fatal("the merge ran before anyone approved it")
	}
	n.human.say("GuildServ", "APPROVE 1")

	wakes := agent.inbox(5)
	if len(wakes) != 1 {
		t.Fatalf("woken with %v", wakes)
	}
	a, _ := wakes[0]["approval"].(map[string]any)
	if a["decision"] != "approved" || !strings.Contains(a["result"].(string), "Merged PR #7") {
		t.Errorf("the decision: %v", a)
	}
	if runs := tools.runs(); len(runs) != 2 || !strings.Contains(runs[1], `"head_sha":"abc123"`) {
		t.Errorf("ran %v; want the merge as approved", runs)
	}
	agent.call("POST", "/wakes/"+wakes[0]["id"].(string)+"/reply", map[string]any{"text": "PASS"})
}

// Agent frameworks load GuildNet's tools as an MCP server.
func TestAnOutsideAgentsToolsWorkOverMCP(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	n.runner.UseTools(&fakeTools{})
	token := outside(t, n)
	client := mcp.NewClient(&mcp.Implementation{Name: "a-langchain-agent", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             "http://" + n.runner.APIAddr() + "/agents/v1/mcp",
		HTTPClient:           &http.Client{Transport: withHeaders{"Authorization": "Bearer " + networkSecret, "GuildNet-Agent-Token": token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"post_message", "show_step", "get_context", "request_approval", "github_read_file", "github_merge_pull_request"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("tools %v should include %s", names, want)
		}
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "post_message", Arguments: map[string]any{"where": "#engineering", "text": "Hello over MCP"}})
	if err != nil || res.IsError {
		t.Fatalf("post_message: %v %+v", err, res)
	}
	n.human.expect("kai's post over MCP", fromContaining("kai", "#engineering", "Hello over MCP"))
	res, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "post_message", Arguments: map[string]any{"where": "#marketing", "text": "x"}})
	if res == nil || !res.IsError {
		t.Error("posting where kai doesn't belong should fail, as a tool error")
	}
}

// withHeaders adds headers to every request, as the platform does.
type withHeaders map[string]string

func (h withHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// An outside agent posts 20 messages a minute at most.
func TestAnOutsideAgentsMessagesAreRateLimited(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	var status int
	for i := 0; i < 21; i++ {
		status, _, _ = agent.call("POST", "/messages", map[string]any{"where": "#engineering", "text": "spam"})
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("the 21st message in a minute: %d", status)
	}
}
