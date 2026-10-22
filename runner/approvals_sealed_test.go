// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/org"
)

// What an agent carries on from after an approval is its paused
// conversation: tool outputs, repo content, connector data. It's kept sealed
// with the network's own secret, bound to the approval it was paused for,
// and dropped once the agent has carried on.

func TestAPausedConversationIsKeptSealed(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	a, err := n.store.Approval(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Resume == "" || strings.Contains(a.Resume, "merge-17") {
		t.Errorf("the paused conversation is kept as %q", a.Resume)
	}

	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
	if r := brain.lastResume(t); r.token != "kai-desk/merge-17" {
		t.Errorf("kai carried on from %q", r.token)
	}
	if a, _ := n.store.Approval(t.Context(), 1); a.Resume != "" {
		t.Errorf("after carrying on, the conversation is still kept: %q", a.Resume)
	}
}

// A conversation moved onto another approval (by whoever can write to the
// database) doesn't open there: approving that one doesn't run the merge.
func TestAConversationMovedToAnotherApprovalWontOpen(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	first, err := n.store.Approval(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	n.human.say("GuildServ", "DENY 1 not yet")
	n.human.expect("kai told", fromContaining("kai", "#engineering", "not merging"))

	now := time.Now()
	if _, err := n.store.RaiseApproval(t.Context(), org.Approval{Agent: "kai", Approver: "method", Route: []string{"method"},
		Tool: "github.add_comment", Summary: "comment on PR #17", Channel: "#engineering", Asker: "method",
		Resume: first.Resume, RaisedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	n.human.say("GuildServ", "APPROVE 2")
	n.human.expect("a confirmation", guildServSaying("method", "approved #2"))

	n.human.expectQuiet("kai carrying on with the merge", 500*time.Millisecond, fromContaining("kai", "#engineering", "merged PR #17"))
	brain.mu.Lock()
	defer brain.mu.Unlock()
	for _, r := range brain.resumes {
		if r.allow {
			t.Errorf("kai carried on from %q, approved for something else", r.token)
		}
	}
}

// A conversation kept before they were sealed is sealed when the runner
// starts, and carried on from as before.
func TestAConversationKeptBeforeSealingIsSealedAtStart(t *testing.T) {
	brain := wantsToMerge()
	n := startNetwork(t, brain)
	now := time.Now()
	if _, err := n.store.RaiseApproval(t.Context(), org.Approval{Agent: "kai", Approver: "method", Route: []string{"method"},
		Tool: "github.merge_pull_request", Summary: "merge PR #17 'Retry webhooks' into main", Channel: "#engineering",
		Asker: "method", Resume: "kai-desk/merge-17", RaisedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	n.restartRunner()
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	var kept string
	for range 100 {
		if a, _ := n.store.Approval(t.Context(), 1); !strings.Contains(a.Resume, "merge-17") {
			kept = a.Resume
			break
		}
		time.Sleep(waitTimeout / 100)
	}
	if kept == "" {
		t.Error("the plain conversation wasn't sealed")
	}

	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
	if r := brain.lastResume(t); r.token != "kai-desk/merge-17" {
		t.Errorf("kai carried on from %q", r.token)
	}
}
