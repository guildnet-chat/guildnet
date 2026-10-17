// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/runner"
)

// Desks of GuildNet's own are counted: before an agent works at one, the
// runner asks GuildNet's platform for a turn (a network's plan has only so
// many desk hours, and desks at once), and after, says how long it worked.
// An agent whose network is out of desk hours works in chat instead, and
// says so; one whose network's desks are all busy waits for one.

// fakeMeter stands in for the platform's broker.
type fakeMeter struct {
	mu      sync.Mutex
	answers []desk.Turn // given in turn; the last one from then on
	fail    error
	asked   []bool // resume, for each ask
	done    map[string]int64
	beats   map[string]int
	beatAt  map[string]time.Time // the last
	doneAt  map[string]time.Time
}

func (m *fakeMeter) BeatDesk(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beats == nil {
		m.beats, m.beatAt = map[string]int{}, map[string]time.Time{}
	}
	m.beats[id]++
	m.beatAt[id] = time.Now()
	return nil
}

func (m *fakeMeter) StartDesk(_ context.Context, agent string, resume bool) (desk.Turn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, resume)
	if m.fail != nil {
		return desk.Turn{}, m.fail
	}
	t := m.answers[0]
	if len(m.answers) > 1 {
		m.answers = m.answers[1:]
	}
	return t, nil
}

func (m *fakeMeter) DeskDone(_ context.Context, id string, seconds int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done == nil {
		m.done, m.doneAt = map[string]int64{}, map[string]time.Time{}
	}
	m.done[id] += seconds
	m.doneAt[id] = time.Now()
	return nil
}

func (m *fakeMeter) reported(id string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.done[id]
	return s, ok
}

func (m *fakeMeter) asks() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.asked...)
}

// meteredDesks is a network whose kai works at a desk of GuildNet's own,
// counted by meter.
func meteredDesks(t *testing.T, meter *fakeMeter, own runner.AgentPlatform, opts ...option) *network {
	t.Helper()
	router := runner.NewRouter(&fakeBrain{reply: replyWith("from chat")}, &fakeBrain{reply: replyWith("from Managed Agents")}).
		WithDesks(own).
		MeterDesks(runner.DeskMetering{Meter: meter, Retry: 50 * time.Millisecond, Wait: 400 * time.Millisecond})
	return startNetworkWith(t, router, append([]option{onPlatform("kai", "desk")}, opts...)...)
}

// A desk waiting for room at a full sandbox says so, and the wait isn't
// counted as its time.
func TestWaitingForRoomAtTheSandboxIsntCounted(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{ID: "turn-1"}}}
	ws := desk.New(&fullSandbox{fullFor: 3}, nil)
	ws.Retry = 500 * time.Millisecond // about 1.5s waiting
	n := meteredDesks(t, meter, roomyBrain{&fakeBrain{reply: replyWith("done")}, ws})

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai saying it's waiting", fromContaining("kai", "#engineering", "waiting for room at the sandbox"))
	n.human.expect("kai answering", fromContaining("kai", "#engineering", "done"))
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if s, ok := meter.reported("turn-1"); ok {
			if s != 1 {
				t.Errorf("reported %ds, want 1: about 1.5s waiting isn't counted, 0.2s working is", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the desk's time was never reported")
		}
	}
}

// roomyBrain readies its desk in ws, then works a moment.
type roomyBrain struct {
	*fakeBrain
	ws *desk.Workspace
}

func (b roomyBrain) Respond(ctx context.Context, req agent.Request, onStep func(agent.Step)) (agent.Result, error) {
	if err := b.ws.Ready(ctx, req.Persona); err != nil {
		return agent.Result{}, err
	}
	time.Sleep(200 * time.Millisecond)
	return b.fakeBrain.Respond(ctx, req, onStep)
}

// fullSandbox has no room for a desk for its first few tries.
type fullSandbox struct {
	mu             sync.Mutex
	fullFor, tries int
}

func (s *fullSandbox) Open(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tries++; s.tries <= s.fullFor {
		return desk.ErrFull
	}
	return nil
}
func (s *fullSandbox) Run(context.Context, string, desk.Command) (desk.Output, error) {
	return desk.Output{}, nil
}
func (s *fullSandbox) Read(context.Context, string, string, int64) ([]byte, error) {
	return nil, desk.ErrNoFile
}
func (s *fullSandbox) Write(context.Context, string, string, []byte) error { return nil }
func (s *fullSandbox) Remove(context.Context, string) error                { return nil }

func TestADesksTimeIsCounted(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{ID: "turn-1"}}}
	n := meteredDesks(t, meter, deskBrain{&fakeBrain{reply: replyWith("done, tests pass")}, 1200 * time.Millisecond})

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai answering from its desk", fromContaining("kai", "#engineering", "done, tests pass"))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s, ok := meter.reported("turn-1"); ok {
			if s != 2 {
				t.Errorf("1.2s of work reported as %ds, want 2 (by the second, rounded up)", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the desk's time was never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if asks := meter.asks(); len(asks) != 1 || asks[0] {
		t.Errorf("asks %v, want one, not resuming", asks)
	}
}

// While a desk works, the runner says so every beat, so the platform can
// tell a desk that's working from one whose runner went away (and charge
// that one up to its last beat). Once it's done, it stops.
func TestAWorkingDeskSaysItsStillWorking(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{ID: "turn-1"}}}
	router := runner.NewRouter(&fakeBrain{reply: replyWith("from chat")}, &fakeBrain{reply: replyWith("from Managed Agents")}).
		WithDesks(deskBrain{&fakeBrain{reply: replyWith("done, tests pass")}, 1200 * time.Millisecond}).
		MeterDesks(runner.DeskMetering{Meter: meter, Retry: 50 * time.Millisecond, Wait: 400 * time.Millisecond, Beat: 100 * time.Millisecond})
	n := startNetworkWith(t, router, onPlatform("kai", "desk"))

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai answering from its desk", fromContaining("kai", "#engineering", "done, tests pass"))
	deadline := time.Now().Add(2 * time.Second)
	for _, ok := meter.reported("turn-1"); !ok; _, ok = meter.reported("turn-1") {
		if time.Now().After(deadline) {
			t.Fatal("the desk's time was never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if meter.beats["turn-1"] < 5 {
		t.Errorf("%d beats in 1.2s of work, every 0.1s; want about 11", meter.beats["turn-1"])
	}
	if meter.beatAt["turn-1"].After(meter.doneAt["turn-1"]) {
		t.Error("it went on beating after the desk was done")
	}
}

func TestOutOfDeskHoursAnAgentWorksInChatAndSaysWhy(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{Refused: "this month's 30 desk hours are used up, and there are no credits"}}}
	own := &fakeBrain{reply: replyWith("from my desk")}
	n := meteredDesks(t, meter, own)

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("why kai isn't at its desk", actionFrom("kai", "#engineering", "this month's 30 desk hours are used up"))
	n.human.expect("kai answering from chat", fromContaining("kai", "#engineering", "from chat"))
	own.mu.Lock()
	defer own.mu.Unlock()
	if len(own.requests) != 0 {
		t.Error("kai's desk worked without a turn")
	}
}

func TestWhenEveryDeskIsBusyAnAgentWaitsForOne(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{Busy: true}, {Busy: true}, {ID: "turn-1"}}}
	n := meteredDesks(t, meter, &fakeBrain{reply: replyWith("from my desk")})

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai saying it's waiting", actionFrom("kai", "#engineering", "waiting for a desk"))
	n.human.expect("then kai answering from its desk", fromContaining("kai", "#engineering", "from my desk"))
}

func TestWhenDesksStayBusyAnAgentWorksInChat(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{Busy: true}}}
	n := meteredDesks(t, meter, &fakeBrain{reply: replyWith("from my desk")})

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("why kai isn't at its desk", actionFrom("kai", "#engineering", "desks stayed busy"))
	n.human.expect("kai answering from chat", fromContaining("kai", "#engineering", "from chat"))
}

// Carrying on after an approval is a desk working too, and counted; the
// platform doesn't refuse it for hours.
func TestCarryingOnAfterAnApprovalIsCounted(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{ID: "turn-1"}, {ID: "turn-2"}}}
	n := meteredDesks(t, meter, wantsToMerge())

	n.human.say("#engineering", "kai: ship it")
	n.human.expect("an approval request", askedToApprove("method"))
	n.human.say("GuildServ", "APPROVE 1")

	n.human.expect("kai carrying on", fromContaining("kai", "#engineering", "merged PR #17"))
	if asks := meter.asks(); len(asks) != 2 || asks[0] || !asks[1] {
		t.Errorf("asks %v, want a wake-up then carrying on", asks)
	}
}

// If the platform can't be asked, an agent still works, but in chat: its
// desk's time can't be counted, so it isn't given away. It tries again for
// a while first, as when every desk is busy.
func TestADeskWaitsThenWorksInChatIfThePlatformCantBeAsked(t *testing.T) {
	meter := &fakeMeter{fail: errors.New("connection refused")}
	n := meteredDesks(t, meter, &fakeBrain{reply: replyWith("from my desk")})

	n.human.say("#engineering", "kai: run the tests")

	n.human.expect("kai answering from chat", fromContaining("kai", "#engineering", "from chat"))
	if asks := meter.asks(); len(asks) < 2 {
		t.Errorf("asked %d times; want it tried again before giving up", len(asks))
	}
}

// Managed Agents' desks run on the company's own Anthropic account, so
// they aren't GuildNet's to count.
func TestManagedAgentsDesksArentCounted(t *testing.T) {
	meter := &fakeMeter{answers: []desk.Turn{{Refused: "out of hours"}}}
	n := meteredDesks(t, meter, &fakeBrain{reply: replyWith("from my desk")}, onPlatform("vera", "managed"))

	n.human.say("#engineering", "vera: run the tests")

	n.human.expect("vera answering from Managed Agents", fromContaining("vera", "#engineering", "from Managed Agents"))
	if asks := meter.asks(); len(asks) != 0 {
		t.Errorf("asked %d times for a Managed Agents desk", len(asks))
	}
}
