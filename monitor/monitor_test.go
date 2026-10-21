// SPDX-License-Identifier: AGPL-3.0-only

package monitor_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// sentry is a stand-in for Sentry's ingest: it keeps the events it's sent.
type sentry struct {
	mu     sync.Mutex
	events []event
	srv    *httptest.Server
}

type event struct {
	Message     string            `json:"message"`
	Level       string            `json:"level"`
	Release     string            `json:"release"`
	Environment string            `json:"environment"`
	ServerName  string            `json:"server_name"`
	Fingerprint []string          `json:"fingerprint"`
	Tags        map[string]string `json:"tags"`
	Request     json.RawMessage   `json:"request"`
	User        json.RawMessage   `json:"user"`
	Exception   json.RawMessage   `json:"exception"`
	Threads     json.RawMessage   `json:"threads"`
	raw         string
}

func newSentry(t *testing.T) *sentry {
	s := &sentry{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			z, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip: %v", err)
				return
			}
			body = z
		}
		// An envelope: a header line, then each item's header and payload.
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		sc.Scan()
		for sc.Scan() {
			var item struct {
				Type string `json:"type"`
			}
			json.Unmarshal(sc.Bytes(), &item)
			if !sc.Scan() {
				break
			}
			if item.Type == "event" {
				var e event
				if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
					t.Errorf("an event that isn't JSON: %v", err)
				}
				e.raw = sc.Text()
				s.mu.Lock()
				s.events = append(s.events, e)
				s.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sentry) dsn() string {
	return "http://publickey@" + strings.TrimPrefix(s.srv.URL, "http://") + "/1"
}

func (s *sentry) sent() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event(nil), s.events...)
}

func start(t *testing.T, o monitor.Options) {
	stop := monitor.Start(o)
	t.Cleanup(stop)
}

func TestAProblemIsLoggedAndReachesSentry(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "platform", Network: "acme", Release: "4b3ca20", Environment: "production"})
	var logged bytes.Buffer
	monitor.Problem(log.New(&logged, "", 0), "finding network %s: %v", "acme", errors.New("dial tcp 172.30.0.3:5432: connection refused"))
	monitor.Flush()

	if got, want := logged.String(), "finding network acme: dial tcp 172.30.0.3:5432: connection refused\n"; got != want {
		t.Errorf("logged %q, want %q", got, want)
	}
	sent := s.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(sent))
	}
	e := sent[0]
	if e.Message != "finding network acme: dial tcp 172.30.0.3:5432: connection refused" || e.Level != "error" {
		t.Errorf("message %q level %q", e.Message, e.Level)
	}
	// Grouped by where it happened, not by what the error said this time.
	if len(e.Fingerprint) != 1 || e.Fingerprint[0] != "finding network %s: %v" {
		t.Errorf("fingerprint %q", e.Fingerprint)
	}
	if e.Tags["service"] != "platform" || e.Tags["network"] != "acme" || e.Release != "4b3ca20" || e.Environment != "production" {
		t.Errorf("tags %v, release %q, environment %q", e.Tags, e.Release, e.Environment)
	}
	// Where in GuildNet's code: this test, not the monitor's own workings.
	if !strings.Contains(string(e.Threads), "TestAProblemIsLoggedAndReachesSentry") || strings.Contains(string(e.Threads), `"module":"github.com/guildnet-chat/guildnet/monitor"`) {
		t.Errorf("stack: %s", e.Threads)
	}
}

// Only what's needed to find the fault goes to Sentry: never people's
// addresses, keys, tokens or passwords, visitors' IP addresses, or the
// request a problem happened in.
func TestSecretsAndPeopleStayOut(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "platform"})
	secrets := []string{
		"ada@example.com",
		"sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"sk-proj-AbCdEfGhIjKlMnOpQrStUv",
		"re_AbCdEfGhIjKlMnOp",
		"gna_AbCdEfGhIjKlMnOpQrStUv",
		"whsec_AbCdEfGhIjKlMnOpQrStUv==",
		"ghs_AbCdEfGhIjKlMnOpQrStUv",
		"AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz",
		"AQ.AbCdEfGhIjKlMnOpQrStUv",
		"hunter2secret",
		"s3cr3tcode",
		"c29tZXRoaW5nLXRoYXQtbG9va3MtbGlrZS1hLXRva2Vu",
		"203.0.113.9",
	}
	monitor.Problem(log.New(io.Discard, "", 0), "emailing %s: %v", "ada@example.com", errors.New(
		"calling with sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789, sk-proj-AbCdEfGhIjKlMnOpQrStUv, re_AbCdEfGhIjKlMnOp, "+
			"gna_AbCdEfGhIjKlMnOpQrStUv, whsec_AbCdEfGhIjKlMnOpQrStUv==, ghs_AbCdEfGhIjKlMnOpQrStUv, AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz, AQ.AbCdEfGhIjKlMnOpQrStUv: "+
			"Authorization: Bearer hunter2secret; GET https://api.example.com/v1/x?code=s3cr3tcode: "+
			"postgres://platform:hunter2secret@postgres:5432/platform; session c29tZXRoaW5nLXRoYXQtbG9va3MtbGlrZS1hLXRva2Vu; from 203.0.113.9")) // trufflehog:ignore (all made up)
	monitor.Flush()

	sent := s.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(sent))
	}
	e := sent[0]
	for _, secret := range secrets {
		if strings.Contains(e.raw, secret) {
			t.Errorf("%q reached Sentry: %s", secret, e.Message)
		}
	}
	// What's left still says what went wrong, and where.
	for _, kept := range []string{"emailing ", "api.example.com/v1/x", "postgres:5432/platform"} {
		if !strings.Contains(e.Message, kept) {
			t.Errorf("message %q lost %q", e.Message, kept)
		}
	}
	if e.ServerName != "" || len(e.Request) > 0 || (len(e.User) > 0 && string(e.User) != "{}") {
		t.Errorf("server %q, request %s, user %s", e.ServerName, e.Request, e.User)
	}
}

// Public IPv6 addresses go too, and secrets in standard base64 (with / in
// them); what helps find a fault (private addresses, paths, times) stays.
func TestScrubbingTakesIPv6AndBase64WithSlashes(t *testing.T) {
	in := "from 2001:db8:85a3::8a2e:370:7334 and [2600:1f18:4a3:6900::1]:443 with key q0b/Xq8Zs+9fT2mK/LpR4wE7yN1vA6cH3uJ5dG0iO8k= " + // trufflehog:ignore (made up)
		"at 09:30:00 via fd12:3456:789a::1 and ::1 in /var/lib/guildnet-broker/shared/ca.crt and open/internal/github/github.go:427"
	got := monitor.Scrub(in)
	for _, gone := range []string{"2001:db8", "2600:1f18", "q0b/Xq8Zs", "LpR4wE7yN1vA6cH3uJ5dG0iO8k"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q kept %q", got, gone)
		}
	}
	for _, kept := range []string{"09:30:00", "fd12:3456:789a::1", "::1", "/var/lib/guildnet-broker/shared/ca.crt", "open/internal/github/github.go:427"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q lost %q", got, kept)
		}
	}
}

// A problem that keeps happening doesn't use up Sentry's quota: from one
// place in the code, it's sent once in a while (it's always logged).
func TestAProblemFromOnePlaceIsSentOnceInAWhile(t *testing.T) {
	s := newSentry(t)
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	start(t, monitor.Options{DSN: s.dsn(), Service: "runner", Every: 10 * time.Minute, Now: func() time.Time { return now }})
	var logged bytes.Buffer
	l := log.New(&logged, "", 0)
	fail := func(i int) { monitor.Problem(l, "send to %s: %v", "#ops", errors.New("broken pipe")) }

	fail(1)
	fail(2)
	monitor.Problem(l, "welcome: %v", errors.New("read-only file system"))
	now = now.Add(11 * time.Minute)
	fail(3)
	monitor.Flush()

	if n := strings.Count(logged.String(), "\n"); n != 4 {
		t.Errorf("logged %d lines, want 4", n)
	}
	var sends int
	for _, e := range s.sent() {
		if e.Fingerprint[0] == "send to %s: %v" {
			sends++
		}
	}
	if sends != 2 || len(s.sent()) != 3 {
		t.Errorf("sent %d of the repeated problem (want 2) and %d in all (want 3)", sends, len(s.sent()))
	}
}

// However many places go wrong at once, only so many go in an hour.
func TestOnlySoManyProblemsAnHour(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "runner", PerHour: 3})
	for _, f := range []string{"a: %v", "b: %v", "c: %v", "d: %v", "e: %v"} {
		monitor.Problem(log.New(io.Discard, "", 0), f, errors.New("x"))
	}
	monitor.Flush()
	if n := len(s.sent()); n != 3 {
		t.Errorf("sent %d, want 3", n)
	}
}

// Without a DSN (a developer's machine, tests, CI) problems are only logged.
func TestWithoutADSNProblemsAreOnlyLogged(t *testing.T) {
	start(t, monitor.Options{Service: "platform"})
	var logged bytes.Buffer
	monitor.Problem(log.New(&logged, "", 0), "welcome: %v", errors.New("x"))
	monitor.Flush()
	if logged.String() != "welcome: x\n" {
		t.Errorf("logged %q", logged.String())
	}
}

// A panic while answering a request is reported with the route it was on
// (never the address it was asked for), and the server carries on as Go's
// does: that request fails, the next is answered.
func TestAPanicInAHandlerIsReported(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "platform"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "boom" {
			var m map[string]int
			m["x"]++ // assignment to entry in nil map
		}
		io.WriteString(w, "ok")
	})
	srv := httptest.NewServer(monitor.Handler(mux))
	defer srv.Close()
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)

	if resp, err := http.Get(srv.URL + "/networks/boom?code=s3cr3tcode"); err == nil {
		resp.Body.Close()
		t.Errorf("the panicking request answered %s", resp.Status)
	}
	resp, err := http.Get(srv.URL + "/networks/fine")
	if err != nil {
		t.Fatalf("after a panic: %v", err)
	}
	resp.Body.Close()
	monitor.Flush()

	sent := s.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(sent))
	}
	e := sent[0]
	if e.Level != "fatal" || !strings.Contains(string(e.Exception), "assignment to entry in nil map") {
		t.Errorf("level %q, exception %s", e.Level, e.Exception)
	}
	if e.Tags["route"] != "GET /networks/{name}" {
		t.Errorf("route %q", e.Tags["route"])
	}
	if strings.Contains(e.raw, "boom") || strings.Contains(e.raw, "s3cr3tcode") {
		t.Errorf("the address reached Sentry: %s", e.raw)
	}
	if !strings.Contains(string(e.Exception), "TestAPanicInAHandlerIsReported") {
		t.Errorf("the stack doesn't say where: %s", e.Exception)
	}
}

// Behind something that copies the request (as signing in does), the route
// isn't known: the first part of the path says roughly where, and nothing
// after it (a network's or person's name) goes.
func TestAPanicBehindMiddlewareSaysRoughlyWhere(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "platform"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /people/{name}", func(http.ResponseWriter, *http.Request) { panic("oops") })
	signedIn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(r.Context()))
	})
	srv := httptest.NewServer(monitor.Handler(signedIn))
	defer srv.Close()
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if resp, err := http.Get(srv.URL + "/people/zebedee42"); err == nil {
		resp.Body.Close()
	}
	monitor.Flush()
	sent := s.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(sent))
	}
	if got := sent[0].Tags["route"]; got != "GET /people/…" {
		t.Errorf("route %q", got)
	}
	if strings.Contains(sent[0].raw, "zebedee42") {
		t.Errorf("the name reached Sentry: %s", sent[0].raw)
	}
}

// Deferred at the top of a goroutine, Recover reports a panic before it
// takes the process down as it would have.
func TestRecoverReportsAPanicThenPanicsOn(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "runner"})
	var carried any
	func() {
		defer func() { carried = recover() }()
		defer monitor.Recover()
		panic("the tool loop went wrong")
	}()
	if carried != "the tool loop went wrong" {
		t.Errorf("the panic carried on as %v", carried)
	}
	sent := s.sent() // Recover waits for it to go: the process is about to end
	if len(sent) != 1 || !strings.Contains(string(sent[0].Exception), "the tool loop went wrong") {
		t.Errorf("sent %+v", sent)
	}
}
