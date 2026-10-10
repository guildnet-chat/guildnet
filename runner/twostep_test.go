// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/guildnet-chat/guildnet/runner"
)

// Two-step sign-in guards the console and its web chat. Plain IRC signs in
// with the password alone, so someone with only an owner's or an admin's
// password could change the org through GuildServ around the second step:
// for those who sign in with two steps (or must), GuildServ takes changes
// only from the console, which has seen both; over IRC they can still look.

// twoSteps is how people sign in to the console, as the platform says.
type twoSteps struct {
	mu   sync.Mutex
	of   map[string]runner.TwoStep
	fail error
}

func (s *twoSteps) TwoStepOf(_ context.Context, account string) (runner.TwoStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.of[strings.ToLower(account)], s.fail
}

func TestTwoStepAccountsChangeTheOrgOnlyFromTheConsole(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	// The network wants two steps of its owner, method.
	n.runner.UseTwoSteps(&twoSteps{of: map[string]runner.TwoStep{"method": {Required: true}}})

	n.human.say("GuildServ", `HIRE rex ROLE "Support" TEAM Engineering`)
	n.human.expect("a change over IRC refused", guildServSaying("method", "console"))
	n.human.say("GuildServ", "INFO kai")
	n.human.expect("looking still works", guildServSaying("method", "kai"))
	n.human.say("#engineering", "!pause")
	n.human.expect("pausing over IRC refused", fromContaining("ledger", "#engineering", "console"))

	if status, out := n.api("method").call("POST", "/v1/guildserv", map[string]any{"text": `HIRE rex ROLE "Support" TEAM Engineering`}); status != http.StatusAccepted {
		t.Fatalf("through the console: %d %v", status, out)
	}
	n.human.expect("hired through the console, answered as GuildServ", guildServSaying("method", "hired rex"))
}

// Someone who's turned two steps on is held to it, whatever their access.
func TestSomeoneWithTwoStepOnChangesThingsOnlyFromTheConsole(t *testing.T) {
	n := startNetwork(t, wantsToMerge(), withAPI())
	n.runner.UseTwoSteps(&twoSteps{of: map[string]runner.TwoStep{"method": {On: true}}})
	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))

	n.human.say("GuildServ", "APPROVE 1")
	n.human.expect("approving over IRC refused", guildServSaying("method", "console"))

	n.api("method").call("POST", "/v1/guildserv", map[string]any{"text": "APPROVE 1"})
	n.human.expect("approved through the console", guildServSaying("method", "approved #1"))
}

// Without two steps (not wanted, not on), IRC works as it always has.
func TestOthersChangeTheOrgOverIRCAsBefore(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.runner.UseTwoSteps(&twoSteps{})

	n.human.say("GuildServ", `HIRE rex ROLE "Support" TEAM Engineering`)

	n.human.expect("hired", guildServSaying("method", "hired rex"))
}

// If how someone signs in can't be told, a change over IRC waits: it's
// refused, not let through.
func TestAChangeOverIRCWaitsIfTwoStepCantBeChecked(t *testing.T) {
	n := startNetwork(t, &fakeBrain{})
	n.runner.UseTwoSteps(&twoSteps{fail: errors.New("the platform didn't answer")})

	n.human.say("GuildServ", `HIRE rex ROLE "Support" TEAM Engineering`)

	n.human.expect("refused", guildServSaying("method", "couldn't check"))
}

// How someone signs in is asked of the platform's broker, as the network.
func TestHowSomeoneSignsInIsAskedOfTheBroker(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer net-secret" || r.Header.Get("GuildNet-Network") != "acme" || r.URL.Path != "/two-step" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"required": true, "on": r.URL.Query().Get("account") == "dana"})
	}))
	defer broker.Close()
	b := runner.BrokerTwoSteps{URL: broker.URL, Network: "acme", Secret: "net-secret"}

	if got, err := b.TwoStepOf(context.Background(), "dana"); err != nil || got != (runner.TwoStep{Required: true, On: true}) {
		t.Errorf("dana: %+v %v", got, err)
	}
	if _, err := (runner.BrokerTwoSteps{URL: broker.URL, Network: "acme", Secret: "wrong"}).TwoStepOf(context.Background(), "dana"); err == nil {
		t.Error("a broker that refused should be an error, not an answer")
	}
}

// The console's way in is for people only: an agent's token, or no actor,
// gets nothing.
func TestOnlyPeopleSendGuildServCommandsThroughTheConsole(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())

	if status, _ := n.api("nobody").call("POST", "/v1/guildserv", map[string]any{"text": "FIRE kai"}); status != http.StatusForbidden {
		t.Errorf("as someone not in the org: %d, want 403", status)
	}
}
