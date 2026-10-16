// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/pki"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// sandboxd on a desks host of its own has a certificate from the console's
// CA that the console can't renew there: it says when it runs out, and,
// three weeks before, that it needs a new one.

// remoteSandboxd is sandboxd over TLS, with a certificate from a CA of the
// console's that runs out after left; and how the console reaches it.
func remoteSandboxd(t *testing.T, left time.Duration) platform.Sandbox {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Certificates last a year from when they're issued.
	if err := ca.Issue(dir, "sandboxd", []string{"127.0.0.1"}, time.Now().Add(left-365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerTLS(filepath.Join(dir, "sandboxd.crt"), filepath.Join(dir, "sandboxd.key")))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "a token is needed", http.StatusUnauthorized)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return platform.Sandbox{URL: "https://127.0.0.1:" + port, Key: SandboxKey, Through: pki.ClientTransport(filepath.Join(dir, "ca.crt"))}
}

func TestTheServerPageSaysWhenSandboxdsCertificateRunsOut(t *testing.T) {
	for _, c := range []struct {
		left  time.Duration
		warns bool
	}{{200 * 24 * time.Hour, false}, {10 * 24 * time.Hour, true}} {
		sb := remoteSandboxd(t, c.left)
		w := NewSelfHostedWorld(t, func(o *Setup) { o.Sandbox = sb })
		_, ada := w.SetUp()

		page := ada.Get("/server")

		until := time.Now().Add(c.left).UTC().Format("2 January 2006")
		if !strings.Contains(page.Text(), "certificate until "+until) {
			t.Errorf("%v left: the page should say sandboxd's certificate lasts until %s:\n%s", c.left, until, page.Text())
		}
		if warns := strings.Contains(page.Text(), "issue it a new one"); warns != c.warns {
			t.Errorf("%v left: the page warns: %v, want %v:\n%s", c.left, warns, c.warns, page.Text())
		}
	}
}

// Desks are on once the runner has them, not when the console has a
// sandbox: until the runner picks it up (it asks every minute), agents
// can't be moved to desks, and the Server page says so.
func TestTheServerPageSaysDesksAreOnOnlyOnceTheRunnerHasThem(t *testing.T) {
	sb := remoteSandboxd(t, 200*24*time.Hour)
	w := NewSelfHostedWorld(t, func(o *Setup) { o.Sandbox = sb })
	_, ada := w.SetUp()
	desks := regexp.MustCompile(`Desks \(agents(?:'|&#39;) workspaces\)</td><td><span[^>]*>([^<]*)<`)

	waiting := desks.FindStringSubmatch(ada.Get("/server").Body)
	w.Stack.Offer("desk")
	on := desks.FindStringSubmatch(ada.Get("/server").Body)

	if waiting == nil || !strings.Contains(waiting[1], "waiting for the runner") {
		t.Errorf("before the runner has desks, the page should say desks wait for it: %q", waiting)
	}
	if on == nil || on[1] != "On" {
		t.Errorf("once the runner has desks, the page should say they're on: %q", on)
	}
}

func TestTheConsoleLogsThatSandboxdsCertificateRunsOutSoon(t *testing.T) {
	for _, c := range []struct {
		left  time.Duration
		warns bool
	}{{200 * 24 * time.Hour, false}, {10 * 24 * time.Hour, true}} {
		var logged bytes.Buffer
		p := platform.New(platform.Options{BaseDomain: "guildnet.test", Sandbox: remoteSandboxd(t, c.left), Logger: log.New(&logged, "", 0)})
		ran := false
		for _, j := range p.Jobs() {
			if j.Every == 24*time.Hour {
				j.Run(context.Background())
				ran = true
			}
		}
		if !ran {
			t.Fatal("no daily job checks sandboxd's certificate")
		}
		if warns := strings.Contains(logged.String(), "sandboxd's certificate runs out"); warns != c.warns {
			t.Errorf("%v left: logged a warning: %v, want %v:\n%s", c.left, warns, c.warns, logged.String())
		}
	}
}
