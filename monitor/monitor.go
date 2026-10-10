// SPDX-License-Identifier: AGPL-3.0-only

// Package monitor tells GuildNet's team about faults in GuildNet's own
// services, through Sentry: panics, and errors the code calls problems.
//
// Only what's needed to find a fault goes: what went wrong (scrubbed of
// email addresses, keys, tokens, passwords and public IP addresses), where
// in the code, and which service, network and release. Never a request,
// cookie or visitor, and never anyone's messages. Without a DSN (a
// developer's machine, tests, CI) problems are only logged.
package monitor

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
)

// Options says where problems go and what they're labelled with.
type Options struct {
	DSN         string // Sentry's DSN for GuildNet; "" is off
	Service     string // platform, runner, host or sandboxd
	Network     string // the network a runner runs, if it's one
	Release     string // which build
	Environment string // e.g. production
	// Every is how often a problem from one place in the code is sent
	// (default 10 minutes), and PerHour how many are sent an hour in all
	// (default 60), so one fault can't use up Sentry's quota.
	Every   time.Duration
	PerHour int
	Now     func() time.Time
}

// FromEnv is the options in the environment: GUILDNET_SENTRY_DSN,
// GUILDNET_NETWORK, GUILDNET_RELEASE (or the release built in) and
// GUILDNET_ENVIRONMENT (default production).
func FromEnv(service string) Options {
	o := Options{
		DSN:         os.Getenv("GUILDNET_SENTRY_DSN"),
		Service:     service,
		Network:     os.Getenv("GUILDNET_NETWORK"),
		Release:     release,
		Environment: os.Getenv("GUILDNET_ENVIRONMENT"),
	}
	if r := os.Getenv("GUILDNET_RELEASE"); r != "" {
		o.Release = r
	}
	if o.Environment == "" {
		o.Environment = "production"
	}
	return o
}

// release is the build's, set when it's built:
// -ldflags "-X github.com/guildnet-chat/guildnet/monitor.release=...".
var release string

type monitor struct {
	o      Options
	client *sentry.Client
	hub    *sentry.Hub
	mu     sync.Mutex
	last   map[string]time.Time // each place's last send
	hour   []time.Time          // sends in the last hour
}

var current atomic.Pointer[monitor]

// Start sends problems where o says, until stop, which sends what's left.
func Start(o Options) (stop func()) {
	if o.DSN == "" {
		current.Store(nil)
		return func() {}
	}
	if o.Every == 0 {
		o.Every = 10 * time.Minute
	}
	if o.PerHour == 0 {
		o.PerHour = 60
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:            o.DSN,
		Release:        o.Release,
		Environment:    o.Environment,
		MaxBreadcrumbs: -1,
		BeforeSend:     scrubbed,
		// Its newer buffer can leave an event unsent by Flush, which Recover
		// relies on as the process ends: the plain transport waits for it.
		DisableTelemetryBuffer: true,
	})
	if err != nil {
		log.Printf("monitor: Sentry isn't usable (%v): problems are only logged", err)
		current.Store(nil)
		return func() {}
	}
	scope := sentry.NewScope()
	scope.SetTag("service", o.Service)
	if o.Network != "" {
		scope.SetTag("network", o.Network)
	}
	m := &monitor{o: o, client: client, hub: sentry.NewHub(client, scope), last: map[string]time.Time{}}
	current.Store(m)
	return func() {
		current.CompareAndSwap(m, nil)
		client.Flush(5 * time.Second)
		client.Close()
	}
}

// Flush waits (a little) for what's been reported to go.
func Flush() {
	if m := current.Load(); m != nil {
		m.client.Flush(5 * time.Second)
	}
}

// Problem logs a fault as l.Printf would (if there's l), and reports it. The format says
// where it happened: reports are grouped by it.
func Problem(l *log.Logger, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if l != nil {
		l.Output(2, line)
	}
	m := current.Load()
	if m == nil || !m.allowed(format) {
		return
	}
	e := sentry.NewEvent()
	e.Level = sentry.LevelError
	e.Message = line
	e.Fingerprint = []string{format}
	e.Threads = []sentry.Thread{{Stacktrace: stack(), Current: true}}
	m.hub.CaptureEvent(e)
}

// allowed says whether a report from this place may go now, and counts it.
func (m *monitor) allowed(place string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.o.Now()
	if last, ok := m.last[place]; ok && now.Sub(last) < m.o.Every {
		return false
	}
	recent := m.hour[:0]
	for _, t := range m.hour {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	m.hour = recent
	if len(m.hour) >= m.o.PerHour {
		return false
	}
	m.hour = append(m.hour, now)
	m.last[place] = now
	return true
}

func (m *monitor) panicked(v any, tags map[string]string) {
	if !m.allowed(fmt.Sprintf("panic %T %v", v, v)) {
		return
	}
	e := sentry.NewEvent()
	e.Level = sentry.LevelFatal
	e.Message = fmt.Sprint(v)
	e.Exception = []sentry.Exception{{
		Type:       fmt.Sprintf("panic: %T", v),
		Value:      fmt.Sprint(v),
		Stacktrace: stack(),
		Mechanism:  &sentry.Mechanism{Type: "panic", Handled: new(false)},
	}}
	e.Tags = tags
	m.hub.CaptureEvent(e)
}

// Handler reports a panic while answering a request, with the route it
// was on, then lets it carry on to net/http as before.
func Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if m := current.Load(); m != nil && v != http.ErrAbortHandler {
				route := r.Pattern
				if route == "" {
					// Routed on a copy of the request: roughly where, and
					// nothing after (names).
					first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
					route = r.Method + " /" + first + "/…"
				}
				m.panicked(v, map[string]string{"route": route})
			}
			panic(v)
		}()
		h.ServeHTTP(w, r)
	})
}

// Recover, deferred at the top of a goroutine, reports a panic and waits
// for the report to go, then panics on: the process ends as it would have.
func Recover() {
	v := recover()
	if v == nil {
		return
	}
	if m := current.Load(); m != nil {
		m.panicked(v, nil)
		m.client.Flush(5 * time.Second)
	}
	panic(v)
}

// stack is where the caller is, leaving out this package and Go's own
// panicking.
func stack() *sentry.Stacktrace {
	st := sentry.NewStacktrace()
	if st == nil {
		return nil
	}
	frames := st.Frames[:0]
	for _, f := range st.Frames {
		if f.Module == "github.com/guildnet-chat/guildnet/monitor" || f.Module == "runtime" && strings.HasPrefix(f.Function, "gopanic") {
			continue
		}
		frames = append(frames, f)
	}
	st.Frames = frames
	return st
}

// scrubbed is the event with nothing in it about anyone, or any secret.
func scrubbed(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.Request = nil
	e.User = sentry.User{}
	e.ServerName = ""
	e.Breadcrumbs = nil
	e.Message = Scrub(e.Message)
	for i := range e.Exception {
		e.Exception[i].Value = Scrub(e.Exception[i].Value)
	}
	return e
}

var scrubs = []struct {
	re   *regexp.Regexp
	with string
}{
	// A URL's user and password, and its query.
	{regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`), "://[removed]@"},
	{regexp.MustCompile(`(https?://[^\s?#"']+)\?[^\s"']*`), "$1?[removed]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[^\s,;"']+`), "$1 [removed]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|secret|token|key|code|signature|sig)=[^&\s"']+`), "$1=[removed]"},
	// Keys and tokens by their look: providers', Resend's, GuildNet's agent
	// tokens and webhook secrets, GitHub's, Google's.
	{regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{8,}|re_[A-Za-z0-9_]{8,}|gna_[A-Za-z0-9_-]{8,}|whsec_[A-Za-z0-9+/=_-]{8,}|gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}|AIza[0-9A-Za-z_-]{16,}|AQ\.[A-Za-z0-9_-]{8,}|xox[abprs]-[A-Za-z0-9-]{8,})`), "[key]"},
	// Anything else long and opaque.
	{regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}`), "[removed]"},
}

var ipv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// Scrub is s without email addresses, keys, tokens, passwords or public IP
// addresses (private ones, inside GuildNet, help find a fault).
func Scrub(s string) string {
	for _, x := range scrubs {
		s = x.re.ReplaceAllString(s, x.with)
	}
	return ipv4.ReplaceAllStringFunc(s, func(a string) string {
		ip := net.ParseIP(a)
		if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			return a
		}
		return "[ip]"
	})
}
