// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/runner"
)

// Desks reach GitHub and the Go, npm and PyPI registries; the network's
// owner chooses more hosts, by name. Anyone else asks: a person in the
// console, or an agent whose desk was refused one. It goes to the owner,
// and only the owner, as an approval. Each change is in the org log, and
// the platform is told (it emails the owner).

// listedHosts are the hosts on the network's list, with who added each and
// who asked for it.
func listedHosts(t *testing.T, n *network) map[string]string {
	t.Helper()
	_, out := n.api("method").call("GET", "/v1/desk-hosts", nil)
	got := map[string]string{}
	list, _ := out["hosts"].([]any)
	for _, x := range list {
		h := x.(map[string]any)
		asked, _ := h["asked"].(string)
		got[fmt.Sprint(h["host"])] = fmt.Sprintf("by %v, asked by %s", h["by"], asked)
	}
	return got
}

func orgLog(t *testing.T, n *network) string {
	t.Helper()
	_, out := n.api("method").call("GET", "/v1/log", nil)
	var lines []string
	for _, x := range out["entries"].([]any) {
		e := x.(map[string]any)
		lines = append(lines, fmt.Sprint(e["actor"], " ", e["action"], " ", e["target"], " ", e["detail"]))
	}
	return strings.Join(lines, "\n")
}

func TestTheOwnerChoosesTheHostsDesksMayReach(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	heard := &notices{}
	n.runner.NotifyWith(heard)

	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/desk-hosts", map[string]any{"host": "API.Stripe.com"})

	if got := listedHosts(t, n); got["api.stripe.com"] != "by method, asked by " {
		t.Errorf("the list: %v", got)
	}
	heard.wait(t, "that api.stripe.com was added", func(e runner.Event) bool {
		return e.Type == "desk_host" && e.DeskHost != nil && e.DeskHost.Host == "api.stripe.com" && e.DeskHost.Change == "added" && e.DeskHost.By == "method"
	})
	if log := orgLog(t, n); !strings.Contains(log, "method allow desk host api.stripe.com") {
		t.Errorf("the org log:\n%s", log)
	}

	// Whatever the owner says: names only, public ones, not blocked.
	for host, why := range map[string]string{"10.0.0.5": "not its address", "pastebin.com": "blocked", "metadata.google.internal": "inside a network", "api.stripe.com": "already"} {
		out := expectStatus(t, n, "method", http.StatusBadRequest, "POST", "/v1/desk-hosts", map[string]any{"host": host})
		if msg, _ := out["error"].(string); !strings.Contains(msg, why) {
			t.Errorf("%s: %q, want it to say %q", host, msg, why)
		}
	}

	expectStatus(t, n, "method", http.StatusOK, "DELETE", "/v1/desk-hosts/api.stripe.com", nil)
	if got := listedHosts(t, n); len(got) != 0 {
		t.Errorf("after removing it: %v", got)
	}
	heard.wait(t, "that api.stripe.com was removed", func(e runner.Event) bool {
		return e.Type == "desk_host" && e.DeskHost != nil && e.DeskHost.Host == "api.stripe.com" && e.DeskHost.Change == "removed"
	})
	if log := orgLog(t, n); !strings.Contains(log, "method remove desk host api.stripe.com") {
		t.Errorf("the org log:\n%s", log)
	}
	expectStatus(t, n, "method", http.StatusNotFound, "DELETE", "/v1/desk-hosts/api.stripe.com", nil)
}

func TestSomeoneElseAsksTheOwnerForAHost(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	dana, _ := kaiUnderDana(t, n)

	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/desk-hosts", map[string]any{"host": "api.stripe.com"})
	expectStatus(t, n, "dana", http.StatusOK, "GET", "/v1/desk-hosts", nil)
	expectStatus(t, n, "dana", http.StatusBadRequest, "POST", "/v1/desk-hosts/ask", map[string]any{"host": "pastebin.com", "why": "notes"})

	expectStatus(t, n, "dana", http.StatusOK, "POST", "/v1/desk-hosts/ask", map[string]any{"host": "api.stripe.com", "why": "the payments work"})

	m := n.human.expect("the owner asked", askedToApprove("method"))
	if !strings.Contains(m.text, "dana") || !strings.Contains(m.text, "api.stripe.com") || !strings.Contains(m.text, "the payments work") {
		t.Errorf("the owner was asked %q", m.text)
	}
	// Only the owner decides who desks may reach: not a manager, not dana.
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/approvals/1/approve", nil)
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/approvals/1/approve", nil)

	if got := listedHosts(t, n); got["api.stripe.com"] != "by method, asked by dana" {
		t.Errorf("the list: %v", got)
	}
	dana.expect("dana told", guildServSaying("dana", "api.stripe.com"))
}

// deskKai is kai, at a desk of GuildNet's own.
var deskKai = config.Persona{Nick: "kai", Team: "Engineering", Platform: runner.PlatformDesk}

func TestAnAgentAsksTheOwnerForAHostItsDeskCantReach(t *testing.T) {
	hosts := &runner.DeskHosts{}
	summary, pinned, err := hosts.Prepare(context.Background(), deskKai, "request_host", map[string]any{"host": "api.stripe.com", "why": "to read the webhook docs"})
	if err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{
		reply: func(req agent.Request) string { return "" },
		approval: func(req agent.Request) *agent.ApprovalRequest {
			if req.Persona.Nick != "kai" || !strings.Contains(req.Trigger.Text, "read the docs") {
				return nil
			}
			return &agent.ApprovalRequest{Tool: "request_host", Summary: summary, Resume: "kai-desk/host"}
		},
		afterResume: func(allow bool, reason string) string {
			if allow {
				return "read them: retries are exponential"
			}
			return "ok, not reading them"
		},
	}
	n := startNetwork(t, brain, withAPI())
	n.runner.UseDeskHosts(hosts)
	kaiUnderDana(t, n) // dana decides kai's other approvals

	n.human.say("#engineering", "kai: read the docs")

	m := n.human.expect("the owner asked, not dana", askedToApprove("method"))
	if !strings.Contains(m.text, "api.stripe.com") || !strings.Contains(m.text, "to read the webhook docs") {
		t.Errorf("the owner was asked %q", m.text)
	}
	expectStatus(t, n, "dana", http.StatusForbidden, "POST", "/v1/approvals/1/approve", nil)
	n.human.say("GuildServ", "APPROVE 1")

	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "retries are exponential"))
	if got := listedHosts(t, n); got["api.stripe.com"] != "by method, asked by kai" {
		t.Errorf("the list: %v", got)
	}
	if got := hosts.Hosts(); !slices.Equal(got, []string{"api.stripe.com"}) {
		t.Errorf("the hosts for the network's pass: %v", got)
	}
	// What the tool tells kai, carrying on.
	if out, err := hosts.Run(context.Background(), deskKai, "request_host", pinned); err != nil || !strings.Contains(out, "api.stripe.com") || !strings.Contains(out, "again") {
		t.Errorf("the tool's answer: %q, %v", out, err)
	}
}

func TestOnlyAgentsAtDesksHaveTheToolToAskForAHost(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	hosts := &runner.DeskHosts{}
	n.runner.UseDeskHosts(hosts)

	tools := hosts.Tools(deskKai)
	if len(tools) != 1 || tools[0].Name != "request_host" || !tools[0].Ask {
		t.Errorf("a desk agent's tools: %+v", tools)
	}
	if tools := hosts.Tools(config.Persona{Nick: "vera", Team: "Engineering"}); len(tools) != 0 {
		t.Errorf("an agent in chat (no desk, no proxy) has %+v", tools)
	}
	if !strings.Contains(hosts.Guide(deskKai), "request_host") {
		t.Errorf("a desk agent isn't told when to ask: %q", hosts.Guide(deskKai))
	}

	// What can't be asked for goes back to the agent, not to a person.
	expectStatus(t, n, "method", http.StatusOK, "POST", "/v1/desk-hosts", map[string]any{"host": "api.stripe.com"})
	for host, why := range map[string]string{"1.2.3.4": "not its address", "webhook.site": "blocked", "api.stripe.com": "already", "github.com": "blocked", "api.github.com": "blocked"} {
		if _, _, err := hosts.Prepare(context.Background(), deskKai, "request_host", map[string]any{"host": host, "why": "x"}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want it to say %q", host, err, why)
		}
	}
	if _, _, err := hosts.Prepare(context.Background(), deskKai, "request_host", map[string]any{"host": "static.crates.io"}); err == nil || !strings.Contains(err.Error(), "why") {
		t.Errorf("asked without saying why: %v", err)
	}
}
