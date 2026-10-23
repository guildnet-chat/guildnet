// SPDX-License-Identifier: AGPL-3.0-only

package serve_test

import (
	"context"
	"io"
	"net"
	"net/http"
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
