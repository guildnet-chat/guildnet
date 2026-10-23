// SPDX-License-Identifier: AGPL-3.0-only

// Package serve runs a program's servers until it's told to stop (SIGTERM,
// at a deploy), then lets requests already in flight finish, so the program
// ends by returning: its deferred calls (the fault reports' flush, closing
// its database) run, which an os.Exit (log.Fatal) skips.
package serve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// Server is anything served until it's stopped.
type Server interface {
	// Serve serves until Stop; it returns nil then, or why it couldn't.
	Serve() error
	// Stop stops taking new work, and waits for what's in flight until ctx
	// ends.
	Stop(ctx context.Context) error
}

// HTTP is an HTTP server: on Listener, or on its Addr if that's nil; over
// TLS if it has a TLSConfig (with its certificates there).
type HTTP struct {
	*http.Server
	Listener net.Listener
}

func (h HTTP) Serve() error {
	l := h.Listener
	if l == nil {
		var err error
		if l, err = net.Listen("tcp", h.Addr); err != nil {
			return err
		}
	}
	var err error
	if h.TLSConfig != nil {
		err = h.Server.ServeTLS(l, "", "")
	} else {
		err = h.Server.Serve(l)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (h HTTP) Stop(ctx context.Context) error {
	return h.Server.Shutdown(ctx)
}

// Func is a server made of two functions: run serves until stop.
func Func(run func() error, stop func(ctx context.Context) error) Server {
	return funcs{run, stop}
}

type funcs struct {
	run  func() error
	stop func(ctx context.Context) error
}

func (f funcs) Serve() error                   { return f.run() }
func (f funcs) Stop(ctx context.Context) error { return f.stop(ctx) }

// Signalled is a context that ends when the program is told to stop
// (SIGTERM, or an interrupt).
func Signalled() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Until serves each of servers until ctx ends, then stops them all, giving
// what's in flight up to grace; or, if one stops serving by itself (its
// address taken, say), stops the others and returns why.
func Until(ctx context.Context, grace time.Duration, servers ...Server) error {
	ended := make(chan error, len(servers))
	for _, s := range servers {
		go func() { ended <- s.Serve() }()
	}
	var why error
	left := len(servers)
	select {
	case <-ctx.Done():
	case why = <-ended:
		left--
		if why == nil {
			why = errors.New("a server stopped by itself")
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for _, s := range servers {
		s.Stop(stopCtx)
	}
	for ; left > 0; left-- {
		select {
		case <-ended:
		case <-stopCtx.Done():
			return why
		}
	}
	return why
}

// Every runs work now, then every d, until ctx ends, with ctx for it to stop
// by too: work done daily isn't put off for ever by restarts more often.
func Every(ctx context.Context, d time.Duration, work func(ctx context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		work(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Health is a health check: 200 while the program serves, 503 once it's
// been told to stop (Draining), so a load balancer (Caddy) sends its work to
// another copy before this one stops taking any.
type Health struct{ draining atomic.Bool }

func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.draining.Load() {
		http.Error(w, "stopping", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

// Draining is a context that ends wait after told does: when told ends
// (SIGTERM), h starts saying the program is stopping, and serving goes on
// for wait, for the load balancer to notice, before it stops (Until).
// Meanwhile the HTTP servers among servers close each connection after
// answering, so what's in front opens new ones, which go to another copy
// once this one stops listening.
func Draining(told context.Context, h *Health, wait time.Duration, servers ...Server) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-told.Done()
		h.draining.Store(true)
		for _, s := range servers {
			if web, ok := s.(HTTP); ok {
				web.SetKeepAlivesEnabled(false)
			}
		}
		time.Sleep(wait)
		cancel()
	}()
	return ctx
}
