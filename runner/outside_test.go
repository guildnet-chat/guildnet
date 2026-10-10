// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
)

// An agent built outside GuildNet (a LangChain script, say) joins as an
// employee. GuildNet keeps its presence, when it wakes, and the org chart;
// the agent's program pulls its wake-ups from an inbox and answers them.
// It's reached through the platform, which adds the network's secret; the
// agent's own token says which agent is calling.

// outsider is an outside agent's program, calling the runner as the
// platform passes its calls on.
type outsider struct {
	t     *testing.T
	base  string
	token string
}

func (n *network) outsider(token string) *outsider {
	return &outsider{t: n.t, base: "http://" + n.runner.APIAddr() + "/agents/v1", token: token}
}

func (o *outsider) call(method, path string, body any) (int, map[string]any, []any) {
	o.t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, o.base+path, in)
	req.Header.Set("Authorization", "Bearer "+networkSecret)
	if o.token != "" {
		req.Header.Set("GuildNet-Agent-Token", o.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		o.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var obj map[string]any
	var list []any
	if json.Unmarshal(b, &obj) != nil {
		json.Unmarshal(b, &list)
	}
	return resp.StatusCode, obj, list
}

// inbox waits up to wait seconds for wake-ups.
func (o *outsider) inbox(wait int) []map[string]any {
	o.t.Helper()
	status, obj, _ := o.call("GET", "/inbox?wait="+strconv.Itoa(wait), nil)
	if status != http.StatusOK {
		o.t.Fatalf("inbox: %d %v", status, obj)
	}
	var out []map[string]any
	for _, w := range obj["wakes"].([]any) {
		out = append(out, w.(map[string]any))
	}
	return out
}

// serve answers every wake-up with answer(wake), until the test ends. It
// checks in once first, so the agent is connected when serve returns.
func (o *outsider) serve(answer func(w map[string]any) (path string, body any)) {
	o.inbox(0)
	done := make(chan struct{})
	o.t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			status, obj, _ := o.call("GET", "/inbox?wait=1", nil)
			if status != http.StatusOK {
				return
			}
			wakes, _ := obj["wakes"].([]any)
			for _, w := range wakes {
				wake := w.(map[string]any)
				path, body := answer(wake)
				o.call("POST", "/wakes/"+wake["id"].(string)+path, body)
			}
		}
	}()
}

// outside makes kai an outside agent and returns its token.
func outside(t *testing.T, n *network) string {
	t.Helper()
	if status, out := n.api("method").call("PATCH", "/v1/staff/kai", map[string]any{"platform": "outside"}); status != http.StatusOK {
		t.Fatalf("moving kai outside: %d %v", status, out)
	}
	status, out := n.api("method").call("POST", "/v1/staff/kai/token", nil)
	token, _ := out["token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, "gna_") {
		t.Fatalf("kai's token: %d %v", status, out)
	}
	return token
}

// An agent brought back from outside loses its token (and its webhook):
// sent outside again, it needs a new one, so an old one that leaked
// doesn't come back to life.
func TestBringingAnAgentBackFromOutsideEndsItsToken(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	agent.inbox(0)

	for _, platform := range []string{"chat", "outside"} {
		if status, out := n.api("method").call("PATCH", "/v1/staff/kai", map[string]any{"platform": platform}); status != http.StatusOK {
			t.Fatalf("moving kai to %s: %d %v", platform, status, out)
		}
	}

	if status, _, _ := agent.call("GET", "/inbox?wait=0", nil); status != http.StatusUnauthorized {
		t.Errorf("the old token, once kai's outside again: %d, want 401", status)
	}
}

func TestAnOutsideAgentGetsItsWorkFromItsInboxAndAnswers(t *testing.T) {
	brain := &fakeBrain{}
	n := startNetwork(t, brain, withAPI())
	agent := n.outsider(outside(t, n))
	var mu sync.Mutex
	var got map[string]any
	agent.serve(func(w map[string]any) (string, any) {
		mu.Lock()
		got = w
		mu.Unlock()
		return "/reply", map[string]any{"text": "Yes: 1.4 is tagged and the notes are up."}
	})

	n.human.say("#engineering", "kai: is the release ready?")

	n.human.expect("kai's answer, from outside", fromContaining("kai", "#engineering", "1.4 is tagged"))
	mu.Lock()
	defer mu.Unlock()
	trigger, _ := got["trigger"].(map[string]any)
	prompt, _ := got["prompt"].(map[string]any)
	if got["type"] != "wake" || got["where"] != "#engineering" || trigger["from"] != ownerAccount || trigger["text"] != "kai: is the release ready?" {
		t.Errorf("the wake-up: %v", got)
	}
	if system, _ := prompt["system"].(string); !strings.Contains(system, "You are Kai") {
		t.Errorf("the wake-up should carry the words GuildNet's own agents get: %v", prompt)
	}
	if user, _ := prompt["user"].(string); !strings.Contains(user, "is the release ready?") || got["reply_by"] == nil {
		t.Errorf("prompt.user %q, reply_by %v", user, got["reply_by"])
	}
	if len(brain.requests) != 0 {
		t.Error("GuildNet's own brain shouldn't be asked for an outside agent")
	}
}

// Nobody's program checking in: whoever asked hears so at once.
func TestAnOutsideAgentThatIsntConnectedSaysSo(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	outside(t, n)

	n.human.say("#engineering", "kai: are you there?")

	n.human.expect("that kai isn't connected", fromContaining("kai", "#engineering", "not connected"))
}

// While it works, an outside agent can show what it's doing, and it can
// give up, saying why.
func TestAnOutsideAgentCanShowStepsAndGiveUp(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	agent.serve(func(w map[string]any) (string, any) {
		agent.call("POST", "/wakes/"+w["id"].(string)+"/steps", map[string]any{"text": "checks the release pipeline"})
		return "/fail", map[string]any{"reason": "the CI server is down"}
	})

	n.human.say("#engineering", "kai: is the release ready?")

	// Its steps are like any agent's: its away message as it goes, and in
	// the summary (every step shows only where the team wants that).
	n.human.expect("kai's step", func(h heard) bool {
		return h.from == "kai" && h.away && strings.Contains(h.text, "checks the release pipeline")
	})
	n.human.expect("the summary", actionFrom("kai", "#engineering", "checks 1"))
	n.human.expect("kai giving up", fromContaining("kai", "#engineering", "couldn't answer"))
	n.human.expect("why, in #ops", fromContaining("ledger", "#ops", "the CI server is down"))
}

// What an outside agent says is untrusted: nothing it sends can smuggle in
// IRC commands or CTCP.
func TestWhatAnOutsideAgentSaysIsCleaned(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	agent.serve(func(w map[string]any) (string, any) {
		return "/reply", map[string]any{"text": "\x01ACTION dances\x01 all good\r\nPRIVMSG #general :pwned"}
	})

	n.human.say("#engineering", "kai: status?")

	n.human.expect("kai's cleaned answer", func(h heard) bool {
		return h.from == "kai" && h.to == "#engineering" && strings.Contains(h.text, "all good") && !h.action && !strings.ContainsAny(h.text, "\x01\r")
	})
	n.human.expectQuiet("anything in #general", 300*time.Millisecond, fromContaining("kai", "#general", "pwned"))
}

// An agent's token works only for it, only with the network's secret (only
// through the platform), and stops the moment a new one is made or the
// agent is fired. Only those who may hire it can make one; it's shown once.
func TestAnOutsideAgentsTokenIsItsAloneAndShownOnce(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	token := outside(t, n)
	agent := n.outsider(token)

	if status, me, _ := agent.call("GET", "/me", nil); status != http.StatusOK || me["nick"] != "kai" || me["team"] != "Engineering" {
		t.Errorf("/me: %d %v", status, me)
	}
	noSecret, _ := http.NewRequest("GET", agent.base+"/me", nil)
	noSecret.Header.Set("GuildNet-Agent-Token", token)
	if resp, err := http.DefaultClient.Do(noSecret); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without the network's secret: %v %v", resp.StatusCode, err)
	}
	if status, _, _ := n.outsider("gna_not-a-token").call("GET", "/me", nil); status != http.StatusUnauthorized {
		t.Errorf("a made-up token: %d", status)
	}
	if status, _, _ := agent.call("POST", "/wakes/wk_someone-elses/reply", map[string]any{"text": "hi"}); status != http.StatusNotFound {
		t.Errorf("answering a wake-up that isn't kai's: %d", status)
	}
	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); strings.Contains(jsonText(out), "gna_") {
		t.Error("the token is shown again")
	}

	n.api("method").call("POST", "/v1/members", map[string]any{"account": "dana", "role": "Designer", "team": "Marketing"})
	if status, _ := n.api("dana").call("POST", "/v1/staff/kai/token", nil); status != http.StatusForbidden {
		t.Errorf("dana, who can't hire in Engineering, made kai a token: %d", status)
	}

	n.api("method").call("POST", "/v1/staff/kai/token", nil)
	if status, _, _ := agent.call("GET", "/me", nil); status != http.StatusUnauthorized {
		t.Errorf("the old token still works: %d", status)
	}
	if status, _ := n.api("method").call("POST", "/v1/staff/vera/token", nil); status != http.StatusBadRequest {
		t.Errorf("a token for an agent that isn't outside: %d", status)
	}
}

// An outside agent's model calls happen elsewhere, so a dollar budget can't
// hold it: its wake-ups are capped per day instead (WAKES, 100 by default).
func TestAnOutsideAgentsWakeUpsAreCappedPerDay(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	if status, out := n.api("method").call("PATCH", "/v1/staff/kai", map[string]any{"daily_wakes": 2}); status != http.StatusOK || out["daily_wakes"] != float64(2) {
		t.Fatalf("setting kai's wake-ups: %d %v", status, out)
	}
	answered := 0
	var mu sync.Mutex
	agent.serve(func(w map[string]any) (string, any) {
		mu.Lock()
		answered++
		mu.Unlock()
		return "/reply", map[string]any{"text": "on it"}
	})

	for i := 1; i <= 2; i++ {
		n.human.say("#engineering", "kai: task "+strconv.Itoa(i))
		n.human.expect("kai answering", fromContaining("kai", "#engineering", "on it"))
	}
	n.human.say("#engineering", "kai: task 3")

	n.human.expect("kai off for the day", fromContaining("ledger", "#ops", "wake-ups"))
	mu.Lock()
	defer mu.Unlock()
	if answered != 2 {
		t.Errorf("kai was woken %d times, want 2", answered)
	}
	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); out["wakes_today"] != float64(2) {
		t.Errorf("wakes today = %v", out["wakes_today"])
	}
}

// Whether an outside agent's program is checking in shows on its page (the
// API) and in INFO.
func TestWhetherAnOutsideAgentIsConnectedShows(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	agent := n.outsider(outside(t, n))
	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); out["connected"] != false || out["platform"] != "outside" {
		t.Errorf("before its program checks in: connected %v, platform %v", out["connected"], out["platform"])
	}
	n.human.say("GuildServ", "INFO kai")
	n.human.expect("INFO saying kai isn't connected", guildServSaying(ownerAccount, "outside GuildNet, not connected"))

	agent.inbox(0)

	if _, out := n.api("method").call("GET", "/v1/staff/kai", nil); out["connected"] != true || out["last_seen"] == nil {
		t.Errorf("after: connected %v, last seen %v", out["connected"], out["last_seen"])
	}
	n.human.say("GuildServ", "INFO kai")
	n.human.expect("INFO saying kai is connected", guildServSaying(ownerAccount, "outside GuildNet, connected"))
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// An outside agent that doesn't answer in time couldn't answer; its late
// answer is refused.
func TestAnOutsideAgentThatDoesntAnswerInTimeCouldntAnswer(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI(), func(cfg *config.Runner, _ *[]config.Persona) { cfg.ReplyTimeout = 700 * time.Millisecond })
	agent := n.outsider(outside(t, n))
	late := make(chan string, 1)
	agent.serve(func(w map[string]any) (string, any) {
		late <- w["id"].(string)
		return "/steps", map[string]any{"text": "thinks"}
	})

	n.human.say("#engineering", "kai: quick one?")

	n.human.expect("kai running out of time", fromContaining("kai", "#engineering", "couldn't answer"))
	if status, _, _ := agent.call("POST", "/wakes/"+<-late+"/reply", map[string]any{"text": "too late"}); status != http.StatusNotFound {
		t.Errorf("a late answer: %d", status)
	}
}
