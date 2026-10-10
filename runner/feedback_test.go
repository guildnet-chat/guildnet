// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// #feedback reaches whoever runs the service: everyone is put in it, its
// topic says where what's said there goes, and the platform collects what
// people (not agents or bots) say there, and only there, for its admins.

func withFeedback() option {
	return func(cfg *config.Runner, _ *[]config.Persona) { cfg.FeedbackChannel = "#feedback" }
}

func feedbackLines(t *testing.T, n *network) []map[string]any {
	t.Helper()
	c := n.api("")
	c.header = false
	status, out := c.call("GET", "/v1/feedback", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/feedback: %d %v", status, out)
	}
	var lines []map[string]any
	b, _ := json.Marshal(out["lines"])
	json.Unmarshal(b, &lines)
	return lines
}

func TestWhatPeopleSayInFeedbackIsCollectedAndNothingElse(t *testing.T) {
	brain := &fakeBrain{reply: func(agent.Request) string { return "sure" }}
	n := startNetwork(t, brain, withAPI(), withFeedback())
	n.human.conn.Join("#feedback")
	n.waitForMembers("#feedback", sortedNicks("ledger", "method")...)

	n.human.say("#engineering", "kai: this is private company talk")
	n.human.expect("kai's reply", fromContaining("kai", "#engineering", "sure"))
	n.human.say("#feedback", "the hiring flow was confusing")

	var lines []map[string]any
	deadline := time.Now().Add(waitTimeout)
	for len(lines) == 0 && time.Now().Before(deadline) {
		lines = feedbackLines(t, n)
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) != 1 || lines[0]["nick"] != "method" || lines[0]["text"] != "the hiring flow was confusing" || lines[0]["id"] == "" || lines[0]["at"] == "" {
		t.Fatalf("feedback = %v, want just method's line", lines)
	}
	b, _ := json.Marshal(lines)
	if strings.Contains(string(b), "private company talk") || strings.Contains(string(b), "sure") {
		t.Errorf("feedback includes other channels: %s", b)
	}
}

func TestFeedbackNeedsTheNetworksSecret(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withFeedback())
	c := n.api("")
	c.header, c.token = false, "network-b-secret"
	if status, _ := c.call("GET", "/v1/feedback", nil); status != http.StatusUnauthorized {
		t.Errorf("with another network's secret: %d, want 401", status)
	}
}

func TestEveryoneIsPutInFeedback(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), withFeedback())
	_, out := n.api("method").call("GET", "/v1/me/channels", nil)
	var chans []string
	for _, c := range out["channels"].([]any) {
		chans = append(chans, c.(string))
	}
	if !slices.Contains(chans, "#feedback") {
		t.Errorf("method's channels = %v, want #feedback among them", chans)
	}
}

// #feedback's topic is set when it isn't already what it should be: not on
// every start, which would announce the same topic after every deploy.
func TestFeedbacksTopicIsntSetAgainWhenItsAlreadyRight(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	var mu sync.Mutex
	topic, changes := "", 0
	n.human.conn.AddCallback("332", func(e ircmsg.Message) { // RPL_TOPIC
		if len(e.Params) >= 3 && strings.EqualFold(e.Params[1], "#feedback") {
			mu.Lock()
			topic = e.Params[2]
			mu.Unlock()
		}
	})
	n.human.conn.AddCallback("TOPIC", func(e ircmsg.Message) {
		if len(e.Params) >= 2 && strings.EqualFold(e.Params[0], "#feedback") {
			mu.Lock()
			topic, changes = e.Params[1], changes+1
			mu.Unlock()
		}
	})
	n.human.conn.Join("#feedback")
	state := func() (string, int) { mu.Lock(); defer mu.Unlock(); return topic, changes }
	deadline := time.Now().Add(15 * time.Second)
	for got, _ := state(); !strings.Contains(got, "people who run this service") && time.Now().Before(deadline); got, _ = state() {
		n.human.conn.Send("TOPIC", "#feedback")
		time.Sleep(300 * time.Millisecond)
	}
	_, before := state()

	n.restart()
	time.Sleep(3 * time.Second) // the new runner has synced by now

	if got, after := state(); after != before || !strings.Contains(got, "people who run this service") {
		t.Errorf("after a restart the topic was set %d more times (now %q); it was already right", after-before, got)
	}
}

// On a real Ergo, GuildServ sets #feedback's topic, so anyone there can see
// where what they say goes.
func TestFeedbackSaysWhereItGoes(t *testing.T) {
	n := startRealNetwork(t, &fakeBrain{})
	var mu sync.Mutex
	topic := ""
	n.human.conn.AddCallback("332", func(e ircmsg.Message) { // RPL_TOPIC
		if len(e.Params) >= 3 && strings.EqualFold(e.Params[1], "#feedback") {
			mu.Lock()
			topic = e.Params[2]
			mu.Unlock()
		}
	})
	n.human.conn.Join("#feedback")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n.human.conn.Send("TOPIC", "#feedback")
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		got := topic
		mu.Unlock()
		if strings.Contains(got, "people who run this service") {
			return
		}
	}
	t.Errorf("#feedback's topic = %q, want it to say what's said there goes to the people who run the service", topic)
}
