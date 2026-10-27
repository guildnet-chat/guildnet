// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/runner"
)

// The runner tells the platform what people may want to hear about by
// email: who an approval is waiting on (when it's raised, and each time it
// moves on), and the network reaching its daily spending cap. It's only
// news: the runner never waits on it.

type notices struct {
	mu  sync.Mutex
	got []runner.Event
}

func (f *notices) Notify(_ context.Context, e runner.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return nil
}

func (f *notices) wait(t *testing.T, what string, match func(runner.Event) bool) runner.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		for _, e := range f.got {
			if match(e) {
				f.mu.Unlock()
				return e
			}
		}
		got := append([]runner.Event(nil), f.got...)
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("the platform wasn't told %s; it was told %+v", what, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The news goes to the platform's broker, as the network, with its secret.
func TestNewsGoesToTheBrokerWithTheNetworksSecret(t *testing.T) {
	var got struct {
		path, network, auth string
		body                runner.Event
	}
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		got.path, got.network, got.auth = r.Method+" "+r.URL.Path, r.Header.Get("GuildNet-Network"), r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer broker.Close()
	b := runner.BrokerNews{URL: broker.URL, Network: "acme", Secret: "net-secret"}

	err := b.Notify(context.Background(), runner.Event{Type: "spend_cap", Spend: &runner.SpendEvent{Spent: 5.1, Cap: 5}})

	if err != nil || got.path != "POST /events" || got.network != "acme" || got.auth != "Bearer net-secret" || got.body.Spend == nil || got.body.Spend.Cap != 5 {
		t.Errorf("err %v; the broker got %+v", err, got)
	}
	if err := (runner.BrokerNews{URL: broker.URL + "/nowhere/", Network: "acme", Secret: "x"}).Notify(context.Background(), runner.Event{Type: "x"}); err == nil {
		t.Error("a broker that didn't take it should be an error")
	}
}

// Someone taken out of the org, over IRC or through the console, is news:
// the console forgets what it kept of theirs (passwords, two-step, where
// their alerts went), so whoever's given the nick later starts afresh.
func TestThePlatformIsToldWhenSomeoneLeavesTheOrg(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	heard := &notices{}
	n.runner.NotifyWith(heard)
	n.human.say("GuildServ", `MEMBER ADD dana ROLE "Designer" TEAM Marketing`)
	n.human.expect("dana added", guildServSaying("method", "added dana"))

	n.human.say("GuildServ", "MEMBER REMOVE dana")

	heard.wait(t, "that dana left", func(e runner.Event) bool {
		return e.Type == "member_removed" && e.Member != nil && e.Member.Account == "dana"
	})
}

func TestThePlatformIsToldWhoAnApprovalIsWaitingOn(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)} // a Wednesday
	n := startNetwork(t, wantsToMerge(), withClock(clock))
	dana, sam := kaiUnderDana(t, n)
	heard := &notices{}
	n.runner.NotifyWith(heard)

	n.human.say("#engineering", "kai: ship it")
	dana.expect("dana asked", askedToApprove("dana"))

	e := heard.wait(t, "that dana is asked", func(e runner.Event) bool {
		return e.Type == "approval" && e.Approval != nil && e.Approval.Approver == "dana"
	})
	a := e.Approval
	if a.ID != 1 || a.Agent != "kai" || !strings.Contains(a.Summary, "merge PR #17") || a.Where != "#engineering" || a.Asker != "method" || a.Expires.IsZero() || a.MovedFrom != "" {
		t.Errorf("the approval event = %+v", a)
	}

	clock.Advance(4*time.Hour + time.Minute)
	sam.expect("sam asked next", askedToApprove("sam"))
	heard.wait(t, "that it moved to sam", func(e runner.Event) bool {
		return e.Type == "approval" && e.Approval != nil && e.Approval.Approver == "sam" && e.Approval.MovedFrom == "dana"
	})
}

func TestThePlatformIsToldWhenTheNetworksCapIsReached(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("done"), usage: ledger.Usage{Output: 1_000_000}}
	n := startNetwork(t, brain)
	heard := &notices{}
	n.runner.NotifyWith(heard)

	n.human.say("#engineering", "kai: big task")
	n.human.expect("the budget notice", fromContaining("ledger", "#ops", "used up"))

	e := heard.wait(t, "the cap was reached", func(e runner.Event) bool { return e.Type == "spend_cap" && e.Spend != nil })
	if e.Spend.Cap != n.ledger.Budget() || e.Spend.Spent < e.Spend.Cap {
		t.Errorf("the cap event = %+v, want the network's $%.2f cap, and spent at least that", e.Spend, n.ledger.Budget())
	}
}
