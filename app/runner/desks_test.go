// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// console is the platform's broker as the runner asks it for the sandbox:
// it answers each ask in turn with the next of its answers, then the last.
type console struct {
	mu      sync.Mutex
	answers []answer
	asked   int
}

type answer struct {
	url string
	err error
}

func (c *console) ask(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.answers[min(c.asked, len(c.answers)-1)]
	c.asked++
	return a.url, a.err
}

func (c *console) times() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

// Desks turned on after the runner started (desks.sh, then `docker compose
// up -d`, which leaves the runner as it is), or a console that didn't answer
// at its start: the runner keeps asking until there's a sandbox, then sets
// desks up once, without a restart.
func TestTheRunnerTakesUpDesksTurnedOnAfterItStarted(t *testing.T) {
	c := &console{answers: []answer{{"", nil}, {"", errors.New("connection refused")}, {"https://sandboxd:8443", nil}}}
	got := make(chan string, 4)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	untilSandbox(ctx, c.ask, time.Millisecond, func(url string) { got <- url }, func(error) {})

	select {
	case url := <-got:
		if url != "https://sandboxd:8443" {
			t.Errorf("desks set up at %q", url)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the runner never took up desks, after asking %d times", c.times())
	}
	asked := c.times()
	time.Sleep(20 * time.Millisecond)
	if c.times() != asked || len(got) != 0 {
		t.Errorf("once it has desks, the runner should stop asking (asked %d, then %d) and set them up once (%d more)", asked, c.times(), len(got))
	}
}

// A sandbox there from the start is set up before the runner starts, as
// before: not a minute later.
func TestTheRunnerSetsUpDesksAtOnceWhenThereAreSome(t *testing.T) {
	c := &console{answers: []answer{{"https://sandboxd:8443", nil}}}
	var got []string

	untilSandbox(context.Background(), c.ask, time.Hour, func(url string) { got = append(got, url) }, func(error) {})

	if len(got) != 1 || got[0] != "https://sandboxd:8443" {
		t.Errorf("desks set up: %v, want once, before untilSandbox returns", got)
	}
}

// A runner that's stopping stops asking.
func TestTheRunnerStopsAskingForASandboxWhenItStops(t *testing.T) {
	c := &console{answers: []answer{{"", nil}}}
	ctx, stop := context.WithCancel(context.Background())

	untilSandbox(ctx, c.ask, time.Millisecond, func(string) { t.Error("there was no sandbox to set up") }, func(error) {})
	stop()
	time.Sleep(10 * time.Millisecond)
	asked := c.times()
	time.Sleep(20 * time.Millisecond)

	if c.times() != asked {
		t.Errorf("asked %d times after stopping", c.times()-asked)
	}
}
