// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/netspec"
	"github.com/guildnet-chat/guildnet/platform"
	"github.com/guildnet-chat/guildnet/runner"
)

// SelfHosted is the one network of a self-hosted world, and its secrets
// (the install's settings).
const (
	SelfHosted            = "hq"
	SelfHostedRunnerToken = "hq-runner-secret"
	SelfHostedErgoToken   = "hq-ergo-secret"
	SelfHostedChatKey     = "hq-chat-key-0123456789abcdef"
)

// Stack is a self-hosted install's network, as its stack starts it: its
// IRC server (the world's fake Ergo) answers from the start, and its runner
// waits for its settings from the console's broker, as a real one does,
// starting once first-run setup gives them.
type Stack struct {
	w     *World
	front *httptest.Server // the runner's address, which the install's settings give
	mu    sync.Mutex
	r     *runner.Runner
	ran   NetworkSettings // what the runner started with
	got   NetworkSettings // what the broker said last
	done  bool
	// restarts counts the runner restarting, to take up new settings.
	restarts int
	// brain is the runner's, kept across its restarts.
	brain StubBrain
}

// Offer lets the network's agents work on platform too: the runner has
// desks ("desk") from now on, say.
func (s *Stack) Offer(platform string) {
	s.brain.Offer(platform)
}

// NetworkSettings are what a self-hosted network's runner is told by the
// broker.
type NetworkSettings struct {
	Template, Owner, Company, Timezone string
}

// NewSelfHostedWorld is a self-hosted install before first-run setup: the
// console alone, with no networks yet and no host agent, told about its one
// (hq) by the install's settings; and hq's stack, started.
func NewSelfHostedWorld(t *testing.T, tweaks ...func(*Setup)) *World {
	t.Helper()
	s := platform.NewMemoryStore()
	stack := &Stack{}
	stack.front = httptest.NewServer(http.HandlerFunc(stack.serve))
	t.Cleanup(stack.front.Close)
	w := newWorld(t, func() Store { return s }, nil, func(w *World, o *Setup) {
		o.Host, o.Google = nil, nil
		o.FirstRun = &platform.FirstRun{Network: SelfHosted,
			Addresses: netspec.Addresses{RunnerURL: stack.front.URL, ErgoURL: w.Ergo.Server.URL + "/" + SelfHosted,
				ChatURL: "ws" + strings.TrimPrefix(w.Chat.Server.URL, "http") + "/" + SelfHosted},
			RunnerToken: SelfHostedRunnerToken, ErgoToken: SelfHostedErgoToken, ChatKey: SelfHostedChatKey}
	}, tweaks...)
	w.Ergo.AddNetwork(SelfHosted, SelfHostedErgoToken)
	w.Stack, stack.w = stack, w
	stop := make(chan struct{})
	go stack.wait(stop)
	t.Cleanup(func() {
		close(stop)
		stack.mu.Lock()
		stack.done = true
		stack.mu.Unlock()
	})
	return w
}

// serve passes calls on to the runner once it has started.
func (s *Stack) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	run := s.r
	s.mu.Unlock()
	if run == nil {
		http.Error(w, "the runner is waiting for first-run setup", http.StatusServiceUnavailable)
		return
	}
	u, _ := url.Parse("http://" + run.APIAddr())
	httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
}

// wait asks the broker for the network's settings, as its runner does,
// starts the runner once there are some, and restarts it when they change.
func (s *Stack) wait(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Millisecond):
		}
		got, ok := s.ask()
		if !ok {
			continue
		}
		s.mu.Lock()
		s.got = got
		if s.r != nil && got != s.ran && !s.done {
			s.r.Stop()
			s.r = nil
			s.restarts++
		}
		if s.r == nil && !s.done {
			r, err := startRunner(s.w.T, netspec.Spec{Template: got.Template, Owner: got.Owner, Company: got.Company,
				Timezone: got.Timezone, RunnerToken: SelfHostedRunnerToken}, &s.brain, func() {})
			if err == nil {
				s.r, s.ran = r, got
			}
		}
		s.mu.Unlock()
	}
}

// ask is what the broker says the network's settings are, if anything.
func (s *Stack) ask() (NetworkSettings, bool) {
	s.w.mu.Lock()
	broker := s.w.Broker
	s.w.mu.Unlock()
	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("GuildNet-Network", SelfHosted)
	req.Header.Set("Authorization", "Bearer "+SelfHostedRunnerToken)
	rec := httptest.NewRecorder()
	broker.ServeHTTP(rec, req.WithContext(context.Background()))
	var got NetworkSettings
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		return NetworkSettings{}, false
	}
	return got, true
}

// Move moves the network's runner to a new address, as moving the install
// (from Compose to a cloud, say) does: the old address stops answering, and
// the install's settings, at the console's next start, give the new one.
func (s *Stack) Move() {
	old := s.front
	s.front = httptest.NewServer(http.HandlerFunc(s.serve))
	s.w.T.Cleanup(s.front.Close)
	old.Close()
}

// Started says whether the network's runner has started.
func (s *Stack) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r != nil
}

// Restarts is how many times the runner restarted, to take up new
// settings.
func (s *Stack) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// Settings are what the broker last told the runner.
func (s *Stack) Settings() NetworkSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got
}

var setupCode = regexp.MustCompile(`setup code ([A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4})`)

// SetupCodes are the setup codes the console has printed to its log, in
// order.
func (w *World) SetupCodes() []string {
	var codes []string
	for _, m := range setupCode.FindAllStringSubmatch(w.Logged(), -1) {
		codes = append(codes, m[1])
	}
	return codes
}

// SetupCode is the last setup code the console printed to its log.
func (w *World) SetupCode() string {
	w.T.Helper()
	codes := w.SetupCodes()
	if len(codes) == 0 {
		w.T.Fatalf("the console printed no setup code:\n%s", w.Logged())
	}
	return codes[len(codes)-1]
}

// SetupForm is a good first-run setup form for ada, with code.
func SetupForm(code string) url.Values {
	return url.Values{"code": {code}, "template": {"team"}, "timezone": {"Europe/Paris"}, "nick": {"ada"},
		"password": {"correct-horse-battery"}, "confirm": {"correct-horse-battery"}, "provider": {"anthropic"},
		"model_key": {"sk-ant-api03-test-key-0123456789"}}
}

// WaitSetUp follows the setup page until setting up has finished (or
// failed).
func (b *Browser) WaitSetUp() Response {
	b.T.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := b.Get("/")
		if r.Status != http.StatusOK || !strings.Contains(r.Text(), "Setting up") || time.Now().After(deadline) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SetUp sets the self-hosted world's network up, as ada, and returns ada's
// browser on the home page and on the network's console, signed in.
func (w *World) SetUp() (home, console *Browser) {
	w.T.Helper()
	home = w.Home()
	home.Get("/")
	if r := home.Post("/setup", SetupForm(w.SetupCode())); r.Status != http.StatusSeeOther {
		w.T.Fatalf("setting up: %d %s", r.Status, r.Text())
	}
	if r := home.WaitSetUp(); !strings.Contains(r.Text(), "is ready") {
		w.T.Fatalf("setting up didn't finish: %d %s", r.Status, r.Text())
	}
	r := home.Post("/setup/open", nil)
	u, err := url.Parse(r.Header.Get("Location"))
	if r.Status != http.StatusSeeOther || err != nil || u.Host != SelfHosted+"."+BaseDomain {
		w.T.Fatalf("opening the network: %d to %q", r.Status, r.Header.Get("Location"))
	}
	console = w.Browser(SelfHosted)
	if r := console.Get(u.RequestURI()); r.Status != http.StatusSeeOther {
		w.T.Fatalf("handed in: %d %s", r.Status, r.Text())
	}
	return home, console
}
