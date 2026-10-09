// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/brain"
)

// A self-hosted network's runner gets its settings from the console's
// broker, once first-run setup has made the network: until then it waits.

// settingsBroker answers "not set up" until ready is set, then the
// settings; only to the network's runner.
func settingsBroker(t *testing.T, ready *atomic.Bool, body string) *brain.Broker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settings" || r.Header.Get("GuildNet-Network") != "hq" || r.Header.Get("Authorization") != "Bearer hq-secret" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		if !ready.Load() {
			http.Error(w, `{"error":"the network isn't set up yet"}`, http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &brain.Broker{URL: srv.URL, Network: "hq", Secret: "hq-secret"}
}

func TestTheRunnerWaitsForFirstRunSetup(t *testing.T) {
	var ready atomic.Bool
	b := settingsBroker(t, &ready, `{"template":"team","owner":"ada","company":"hq","timezone":"Europe/Paris"}`)
	var logged bytes.Buffer
	go func() {
		time.Sleep(100 * time.Millisecond)
		ready.Store(true)
	}()

	s, err := waitForSettings(context.Background(), b, 10*time.Millisecond, log.New(&logged, "", 0))

	if err != nil || s != (networkSettings{Template: "team", Owner: "ada", Company: "hq", Timezone: "Europe/Paris"}) {
		t.Fatalf("got %+v, %v", s, err)
	}
	if n := strings.Count(logged.String(), "waiting for first-run setup"); n != 1 {
		t.Errorf("said it was waiting %d times, want once:\n%s", n, logged.String())
	}
}

func TestTheRunnerStartsFromTheTemplateSetupChose(t *testing.T) {
	s := networkSettings{Template: "team", Owner: "ada", Company: "hq", Timezone: "Europe/Paris"}

	path, err := s.configPath("/app/templates")

	if err != nil || path != filepath.Join("/app/templates", "team", "guildnet.yaml") {
		t.Errorf("config: %q, %v", path, err)
	}
	if env := s.env(); env["GUILDNET_OWNER"] != "ada" || env["GUILDNET_COMPANY"] != "hq" || env["GUILDNET_TIMEZONE"] != "Europe/Paris" {
		t.Errorf("env: %v", env)
	}
	for _, bad := range []string{"../../etc", "nope", ""} {
		if _, err := (networkSettings{Template: bad}).configPath("/app/templates"); err == nil {
			t.Errorf("template %q was taken", bad)
		}
	}
}

func TestAWrongSecretIsntWaitedOutQuietly(t *testing.T) {
	var ready atomic.Bool
	b := settingsBroker(t, &ready, `{}`)
	b.Secret = "wrong"

	_, err := fetchSettings(context.Background(), b)

	if err == nil || err == errNotSetUp {
		t.Errorf("a wrong secret: %v", err)
	}
}
