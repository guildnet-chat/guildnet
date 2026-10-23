// SPDX-License-Identifier: AGPL-3.0-only

package serve_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/serve"
)

func listener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Told to stop (SIGTERM, at a deploy), a request already being answered
// is answered, then serving ends without a fault; nothing new is taken.
func TestARequestInFlightIsAnsweredBeforeServingEnds(t *testing.T) {
	l := listener(t)
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})}
	ctx, stop := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- serve.Until(ctx, 5*time.Second, serve.HTTP{Server: srv, Listener: l}) }()

	answer := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + l.Addr().String())
		if err != nil {
			answer <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		answer <- string(b)
	}()
	<-started
	stop()
	time.Sleep(50 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		t.Error("a new connection was taken while stopping")
	}
	close(release)

	if got := <-answer; got != "done" {
		t.Errorf("the request in flight got %q", got)
	}
	if err := <-ended; err != nil {
		t.Errorf("serving ended with %v", err)
	}
}

// A request that outlasts the grace is cut off: serving still ends.
func TestServingEndsAfterTheGrace(t *testing.T) {
	l := listener(t)
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})}
	ctx, stop := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- serve.Until(ctx, 100*time.Millisecond, serve.HTTP{Server: srv, Listener: l}) }()
	go http.Get("http://" + l.Addr().String())
	<-started
	stop()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("serving didn't end after the grace")
	}
}

// One server failing (its address taken) ends them all, and says why.
func TestOneServerFailingEndsThemAll(t *testing.T) {
	taken := listener(t)
	defer taken.Close()
	ok := listener(t)
	srv := &http.Server{Handler: http.NotFoundHandler()}
	ended := make(chan error, 1)
	go func() {
		ended <- serve.Until(context.Background(), time.Second,
			serve.HTTP{Server: srv, Listener: ok},
			serve.HTTP{Server: &http.Server{Addr: taken.Addr().String(), Handler: http.NotFoundHandler()}})
	}()
	select {
	case err := <-ended:
		if err == nil {
			t.Error("serving ended without saying why")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serving went on with one server failed")
	}
	if _, err := net.DialTimeout("tcp", ok.Addr().String(), time.Second); err == nil {
		t.Error("the other server is still taking connections")
	}
}

// The health check says a copy is serving until it's told to stop; then it
// says it isn't, while it goes on serving for a while, so whatever sends it
// work (Caddy) moves to another copy before this one stops taking any.
func TestHealthSaysSoBeforeServingStops(t *testing.T) {
	var h serve.Health
	status := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		return rec.Code
	}
	if got := status(); got != http.StatusOK {
		t.Errorf("serving: %d, want 200", got)
	}
	told, stop := context.WithCancel(context.Background())
	stopping := serve.Draining(told, &h, 200*time.Millisecond)
	stop()
	time.Sleep(20 * time.Millisecond)
	if got := status(); got != http.StatusServiceUnavailable {
		t.Errorf("told to stop: %d, want 503", got)
	}
	select {
	case <-stopping.Done():
		t.Fatal("serving stopped as soon as it was told, with no time for Caddy to move off")
	default:
	}
	select {
	case <-stopping.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("serving never stopped")
	}
}

// While a copy drains, it keeps no connection open after answering: what's
// in front (Caddy, reaching it by a name the new copy has too) opens new
// ones, and once this copy stops listening they go to the new copy, with no
// request caught on a connection that's being closed.
func TestADrainingCopyKeepsNoConnectionOpen(t *testing.T) {
	l := listener(t)
	srv := serve.HTTP{Server: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hi") })}, Listener: l}
	var h serve.Health
	told, stop := context.WithCancel(context.Background())
	stopping := serve.Draining(told, &h, time.Second, srv)
	ended := make(chan error, 1)
	go func() { ended <- serve.Until(stopping, time.Second, srv) }()
	get := func() *http.Response {
		resp, err := http.Get("http://" + l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := get(); resp.Close {
		t.Error("serving, it closed the connection after one answer")
	}
	stop()
	time.Sleep(50 * time.Millisecond)
	if resp := get(); !resp.Close {
		t.Error("draining, it kept the connection open")
	}
	if err := <-ended; err != nil {
		t.Errorf("serving ended with %v", err)
	}
}

// Work done every so often is done once at the start too: a daily job
// isn't put off for ever by deploys more often than daily.
func TestWorkEverySoOftenIsDoneAtTheStartToo(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ran := make(chan struct{}, 1)
	go serve.Every(ctx, time.Hour, func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("it waited the whole hour before the first time")
	}
}

// Work done every so often stops when serving does.
func TestWorkEverySoOftenStopsWithServing(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		serve.Every(ctx, 10*time.Millisecond, func(ctx context.Context) { runs.Add(1) })
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("it went on after serving stopped")
	}
	if runs.Load() == 0 {
		t.Error("it never ran")
	}
	after := runs.Load()
	time.Sleep(50 * time.Millisecond)
	if runs.Load() != after {
		t.Error("it ran after stopping")
	}
}

// Other kinds of server (a plain TCP listener) are served the same way.
func TestAnyServerCanBeServed(t *testing.T) {
	var stopped atomic.Bool
	quit := make(chan struct{})
	s := serve.Func(
		func() error { <-quit; return nil },
		func(context.Context) error { stopped.Store(true); close(quit); return nil },
	)
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if err := serve.Until(ctx, time.Second, s); err != nil || !stopped.Load() {
		t.Errorf("ended with %v, stopped %v", err, stopped.Load())
	}
}
