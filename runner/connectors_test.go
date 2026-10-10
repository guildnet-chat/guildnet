// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/connectors"
)

// Connectors (Linear, Notion...) the network's owner connected on the
// platform: which a team uses is set like its repos, by whoever may arrange
// the team; what each tool may do without asking is set by whoever may
// approve anything in the org.

// fakeConnectors is the network's connected services, as the runner sees
// them.
type fakeConnectors struct {
	mu       sync.Mutex
	services []connectors.Service
	set      []string
}

func newFakeConnectors() *fakeConnectors {
	return &fakeConnectors{services: []connectors.Service{
		{ID: "linear", Title: "Linear", Tools: []connectors.ToolState{
			{Name: "create_issue", Description: "Create an issue", Default: "ask", State: "approved"},
			{Name: "search_issues", Description: "Search issues", Default: "allow", State: "approved"},
		}},
		{ID: "notion", Title: "Notion"},
	}}
}

func (f *fakeConnectors) Services() []connectors.Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.services)
}

func (f *fakeConnectors) SetPolicy(_ context.Context, connector, tool, policy string) error {
	if !slices.Contains([]string{"", "allow", "ask", "deny"}, policy) {
		return connectors.ErrBadPolicy
	}
	return f.record(connector, tool, "policy "+policy)
}

func (f *fakeConnectors) Approve(_ context.Context, connector, tool string) error {
	return f.record(connector, tool, "approve")
}

func (f *fakeConnectors) record(connector, tool, what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.services {
		if s.ID == connector && slices.ContainsFunc(s.Tools, func(t connectors.ToolState) bool { return t.Name == tool }) {
			f.set = append(f.set, connector+" "+tool+" "+what)
			return nil
		}
	}
	return connectors.ErrNoTool
}

func TestATeamsLeadChoosesItsConnectors(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	n.runner.UseConnectors(newFakeConnectors())
	withSales(t, n) // dana leads Sales

	n.human.say("GuildServ", "TEAM SET Engineering CONNECTORS linear")
	n.human.expect("Engineering's connectors", guildServSaying("method", "connectors linear"))
	expectStatus(t, n, "dana", http.StatusOK, "PATCH", "/v1/teams/Sales", map[string]any{"connectors": []string{"Notion"}})
	expectStatus(t, n, "dana", http.StatusForbidden, "PATCH", "/v1/teams/Engineering", map[string]any{"connectors": []string{"notion"}})
	out := expectStatus(t, n, "method", http.StatusBadRequest, "PATCH", "/v1/teams/Sales", map[string]any{"connectors": []string{"jira"}})
	if errorOf(out) == "" {
		t.Error("a connector the network hasn't connected should be refused with a reason")
	}

	if got := strs(teamIn(t, n, "Sales")["connectors"]); !slices.Equal(got, []string{"notion"}) {
		t.Errorf("Sales' connectors = %v", got)
	}
	if got := n.runner.ConnectorsFor("Engineering"); !slices.Equal(got, []string{"linear"}) {
		t.Errorf("Engineering uses %v", got)
	}
}

func TestWhatConnectorToolsMayDoIsSetByApprovers(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	f := newFakeConnectors()
	n.runner.UseConnectors(f)
	withSales(t, n) // dana leads Sales: she approves there, not everywhere

	status, out := n.api("dana").call("GET", "/v1/connectors", nil)
	list, _ := out["connectors"].([]any)
	if status != http.StatusOK || len(list) != 2 {
		t.Fatalf("the network's connectors: %d %v", status, out)
	}
	if linear := list[0].(map[string]any); linear["id"] != "linear" || len(linear["tools"].([]any)) != 2 {
		t.Errorf("Linear: %v", linear)
	}

	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/connectors/linear/tools/create_issue", map[string]any{"policy": "allow"})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/connectors/linear/tools/create_issue", map[string]any{"policy": "allow"})
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/connectors/linear/tools/search_issues", map[string]any{"approve": true})
	expectStatus(t, n, "method", http.StatusBadRequest, "POST", "/v1/connectors/linear/tools/create_issue", map[string]any{"policy": "sometimes"})
	expectStatus(t, n, "method", http.StatusNotFound, "POST", "/v1/connectors/linear/tools/delete_everything", map[string]any{"policy": "allow"})

	f.mu.Lock()
	set := strings.Join(f.set, "|")
	f.mu.Unlock()
	if set != "linear create_issue policy allow|linear search_issues approve" {
		t.Errorf("set %q", set)
	}
	_, log := n.api("method").call("GET", "/v1/log", nil)
	var entries []string
	for _, x := range log["entries"].([]any) {
		e := x.(map[string]any)
		entries = append(entries, fmt.Sprint(e["action"], " ", e["target"], " ", e["detail"]))
	}
	if s := strings.Join(entries, "\n"); !strings.Contains(s, "linear create_issue") || !strings.Contains(s, "allow") {
		t.Errorf("the change should be in the org log:\n%s", s)
	}
}
