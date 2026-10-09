// SPDX-License-Identifier: AGPL-3.0-only

package desk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
)

// A runner learns from the platform's broker where its network's sandbox
// is, with its token, and asks the broker to wake the sandbox VM.

func broker(t *testing.T, sandboxHere bool) (*desk.Broker, *int) {
	t.Helper()
	wakes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer initech-secret" || r.Header.Get("GuildNet-Network") != "initech" {
			http.Error(w, `{"error":"the network's secret is required"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case !sandboxHere:
			http.Error(w, `{"error":"there's no sandbox here"}`, http.StatusNotFound)
		case r.Method == "GET" && r.URL.Path == "/sandbox":
			w.Write([]byte(`{"url":"http://10.20.0.5:8443","token":"initech.sig"}`))
		case r.Method == "POST" && r.URL.Path == "/sandbox/wake":
			wakes++
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &desk.Broker{URL: srv.URL, Network: "initech", Secret: "initech-secret"}, &wakes
}

func TestTheBrokerSaysWhereTheSandboxIs(t *testing.T) {
	b, _ := broker(t, true)

	url, token, err := b.Sandbox(context.Background())

	if err != nil || url != "http://10.20.0.5:8443" || token != "initech.sig" {
		t.Errorf("%q %q %v", url, token, err)
	}
}

func TestWithoutASandboxThereAreNoDesks(t *testing.T) {
	b, _ := broker(t, false)

	url, token, err := b.Sandbox(context.Background())

	if err != nil || url != "" || token != "" {
		t.Errorf("%q %q %v; want nothing, and no error", url, token, err)
	}
}

// A desk's token is good for a few minutes: the runner keeps one until
// shortly before it runs out, then asks the broker for another, and asks
// again at once if sandboxd won't take the one it has.
func TestADesksTokenIsRenewedBeforeItRunsOut(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		n := asked
		mu.Unlock()
		fmt.Fprintf(w, `{"url":"http://sandbox","token":"t%d","expires":%q}`, n, time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	tokens := (&desk.Broker{URL: srv.URL, Network: "initech", Secret: "s"}).Tokens()
	ctx := context.Background()

	first, err := tokens(ctx, false)
	again, _ := tokens(ctx, false)
	fresh, _ := tokens(ctx, true)

	if err != nil || first != "t1" || again != "t1" {
		t.Errorf("first %q, again %q (%v): want one token, kept", first, again, err)
	}
	if fresh != "t2" {
		t.Errorf("asked for a fresh one: %q, want t2", fresh)
	}
}

// sandboxd refusing a token (it ran out, or the key changed) gets a fresh
// one asked for, and the call made again, once.
func TestARefusedDeskTokenIsReplacedOnce(t *testing.T) {
	sandboxd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, `{"error":"that isn't a good sandbox token"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sandboxd.Close()
	var gave []string
	source := func(_ context.Context, fresh bool) (string, error) {
		token := "stale"
		if fresh {
			token = "good"
		}
		gave = append(gave, token)
		return token, nil
	}

	if err := desk.NewRemoteFrom(sandboxd.URL, source).Open(context.Background(), "kai"); err != nil {
		t.Errorf("after a fresh token: %v", err)
	}
	if strings.Join(gave, ",") != "stale,good" {
		t.Errorf("tokens given: %v", gave)
	}
	never := func(context.Context, bool) (string, error) { return "stale", nil }
	if err := desk.NewRemoteFrom(sandboxd.URL, never).Open(context.Background(), "kai"); err == nil {
		t.Error("a token refused twice should be an error")
	}
}

func TestTheBrokerWakesTheSandbox(t *testing.T) {
	b, wakes := broker(t, true)

	if err := b.Wake(context.Background()); err != nil || *wakes != 1 {
		t.Errorf("%v, woken %d times", err, *wakes)
	}
}
