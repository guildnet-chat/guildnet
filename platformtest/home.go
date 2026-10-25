// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/ircfake"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/netspec"
	"github.com/guildnet-chat/guildnet/platform"
	"github.com/guildnet-chat/guildnet/runner"
)

// The home page (the base domain): signing in with Google, and creating
// networks. Google and the host are faked; the networks the fake host
// "creates" are real runners, started from the real templates.

const (
	googleClientID     = "guildnet-test.apps.googleusercontent.com"
	googleClientSecret = "google-client-secret"
)

// FakeGoogle is Google's OpenID Connect endpoints: the test plays the person
// choosing their account (approve), and the platform exchanges the code.
type FakeGoogle struct {
	t      *testing.T
	Server *httptest.Server
	mu     sync.Mutex
	// codes are what approve handed out, by code.
	codes map[string]googleGrant
	// IDToken, when set, changes the claims Google returns (to test
	// refusals).
	IDToken func(claims map[string]any)
}

type googleGrant struct {
	redirectURI, challenge, nonce, sub, email string
}

func newFakeGoogle(t *testing.T) *FakeGoogle {
	g := &FakeGoogle{t: t, codes: map[string]googleGrant{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.token))
	t.Cleanup(g.Server.Close)
	return g
}

// Options are the console's Google sign-in settings, pointed at the fake.
func (g *FakeGoogle) Options() *platform.GoogleSignIn {
	return &platform.GoogleSignIn{ClientID: googleClientID, ClientSecret: googleClientSecret,
		AuthURL: g.Server.URL + "/auth", TokenURL: g.Server.URL + "/token"}
}

// Approve is the person picking their Google account on Google's page,
// which the platform sent them to (authURL). It returns where Google sends
// them back.
func (g *FakeGoogle) Approve(authURL, email, sub string) string {
	g.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, g.Server.URL+"/auth") {
		g.t.Fatalf("the platform sent the person to %q, not Google", authURL)
	}
	q := u.Query()
	if q.Get("client_id") != googleClientID || q.Get("response_type") != "code" || !strings.Contains(q.Get("scope"), "openid") ||
		!strings.Contains(q.Get("scope"), "email") || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		g.t.Fatalf("not a proper sign-in request: %v", q)
	}
	code := "code-" + sub + "-" + q.Get("state")[:8]
	g.mu.Lock()
	g.codes[code] = googleGrant{redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), sub: sub, email: email}
	g.mu.Unlock()
	back, _ := url.Parse(q.Get("redirect_uri"))
	back.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	return back.String()
}

func (g *FakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" || r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	g.mu.Lock()
	grant, ok := g.codes[r.PostForm.Get("code")]
	delete(g.codes, r.PostForm.Get("code")) // a code works once
	g.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || r.PostForm.Get("client_id") != googleClientID || r.PostForm.Get("client_secret") != googleClientSecret ||
		r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != grant.redirectURI ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	claims := map[string]any{"iss": "https://accounts.google.com", "aud": googleClientID, "sub": grant.sub, "email": grant.email,
		"email_verified": true, "nonce": grant.nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	if g.IDToken != nil {
		g.IDToken(claims)
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3599,
		"id_token": enc(header) + "." + enc(payload) + "." + enc([]byte("signature"))})
}

// FakeHost stands in for the machine's host agent: putting a network
// starts a real runner from the real template on a fake IRC server, and
// tells the fake Ergo about the network's secret.
type FakeHost struct {
	w       *World
	mu      sync.Mutex
	Specs   map[string]netspec.Spec
	Running map[string]netspec.Addresses
	Stopped map[string]bool // suspended networks
	Removed []string
	Fail    error
	held    chan struct{} // while set, Remove waits for it to close
	puts    chan struct{} // while set, Put waits for it to close
	// refreshes counts each network's runner being asked to fetch its model
	// keys again.
	refreshes map[string]int
}

// KeyRefreshes is how many times network's runner was asked to fetch its
// model keys again.
func (h *FakeHost) KeyRefreshes(network string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refreshes[network]
}

func (h *FakeHost) Stop(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.Running[name]; !ok {
		return errors.New("no network " + name)
	}
	h.Stopped[name] = true
	return nil
}

func (h *FakeHost) Start(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.Running[name]; !ok {
		return errors.New("no network " + name)
	}
	delete(h.Stopped, name)
	return nil
}

// Stats has the machine at 1000 MiB with 400 MiB free, and each running
// network using 40 MiB of its 224.
func (h *FakeHost) Stats(context.Context) (netspec.Stats, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := netspec.Stats{MemoryTotal: 1000 << 20, MemoryAvailable: 400 << 20, Networks: map[string]netspec.NetworkStats{}}
	for name := range h.Running {
		n := netspec.NetworkStats{MemoryLimit: 224 << 20}
		if !h.Stopped[name] {
			n.Running, n.Memory, n.CPU = true, 40<<20, 1.5
		}
		s.Networks[name] = n
	}
	return s, nil
}

// IsStopped says whether the network is stopped: suspended, or asleep.
func (h *FakeHost) IsStopped(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Stopped[name]
}

// WasRemoved says whether the network was removed.
func (h *FakeHost) WasRemoved(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.Removed, name)
}

// Hold makes removals wait until release.
func (h *FakeHost) Hold() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = make(chan struct{})
}

// Release lets removals held by Hold go on.
func (h *FakeHost) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.held)
	h.held = nil
}

var envMu sync.Mutex // templates are filled in from the environment

// HoldStarts makes starting networks wait until ReleaseStarts.
func (h *FakeHost) HoldStarts() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.puts = make(chan struct{})
}

// ReleaseStarts lets starts held by HoldStarts go on.
func (h *FakeHost) ReleaseStarts() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.puts)
	h.puts = nil
}

func (h *FakeHost) Put(_ context.Context, name string, s netspec.Spec) (netspec.Addresses, error) {
	h.mu.Lock()
	if wait := h.puts; wait != nil {
		h.mu.Unlock()
		<-wait
		h.mu.Lock()
	}
	defer h.mu.Unlock()
	if h.Fail != nil {
		return netspec.Addresses{}, h.Fail
	}
	h.Specs[name] = s
	if a, ok := h.Running[name]; ok {
		return a, nil
	}
	r, err := startRunner(h.w.T, s, &StubBrain{}, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.refreshes == nil {
			h.refreshes = map[string]int{}
		}
		h.refreshes[name]++
	})
	if err != nil {
		return netspec.Addresses{}, err
	}
	h.w.Ergo.AddNetwork(name, s.ErgoToken)
	a := netspec.Addresses{RunnerURL: "http://" + r.APIAddr(), ErgoURL: h.w.Ergo.Server.URL + "/" + name,
		ChatURL: "ws" + strings.TrimPrefix(h.w.Chat.Server.URL, "http") + "/" + name}
	h.Running[name] = a
	return a, nil
}

// startRunner starts a network's real runner from the real template that s
// names, on a fake IRC server, as its host would; refreshed is called when
// it's asked to fetch its model keys again.
func startRunner(t *testing.T, s netspec.Spec, brain *StubBrain, refreshed func()) (*runner.Runner, error) {
	irc, err := ircfake.Start()
	if err != nil {
		return nil, err
	}
	t.Cleanup(irc.Close)
	// The network's data directory is its own, not the test's TempDir: a
	// network the platform finishes creating as the test ends mustn't fail
	// the test's cleanup. Removed last, after its runner stops and its
	// ledger closes.
	dataDir, err := os.MkdirTemp("", "guildnet-network-")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { os.RemoveAll(dataDir) })
	envMu.Lock()
	for k, v := range map[string]string{"GUILDNET_OWNER": s.Owner, "GUILDNET_COMPANY": s.Company, "GUILDNET_TIMEZONE": s.Timezone,
		"GUILDNET_DATA_DIR": dataDir, "GUILDNET_SERVER": irc.Addr()} {
		os.Setenv(k, v)
	}
	cfg, personas, err := config.Load(filepath.Join(templatesDir(), s.Template, "guildnet.yaml"))
	for _, k := range []string{"GUILDNET_OWNER", "GUILDNET_COMPANY", "GUILDNET_TIMEZONE", "GUILDNET_DATA_DIR", "GUILDNET_SERVER"} {
		os.Unsetenv(k)
	}
	envMu.Unlock()
	if err != nil {
		return nil, err
	}
	cfg.API.Listen, cfg.API.Token, cfg.ContinueWithin = "127.0.0.1:0", s.RunnerToken, -1
	cfg.RefreshModelKeys = refreshed
	l, err := ledger.Open(cfg.DataDir, cfg.DailyBudgetUSD)
	if err != nil {
		return nil, err
	}
	// Before its directory goes: work still finishing writes nothing there.
	t.Cleanup(l.Close)
	r := runner.New(cfg, org.NewMemory(org.SeedFrom(cfg, personas)), brain, l, log.New(io.Discard, "", 0))
	if err := r.Start(); err != nil {
		return nil, err
	}
	t.Cleanup(r.Stop)
	return r, nil
}

func (h *FakeHost) Remove(_ context.Context, name string) error {
	h.mu.Lock()
	held := h.held
	h.mu.Unlock()
	if held != nil {
		<-held
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Fail != nil {
		return h.Fail
	}
	delete(h.Running, name)
	h.Removed = append(h.Removed, name)
	return nil
}

// Spec is what the network was last put with.
func (h *FakeHost) Spec(name string) (netspec.Spec, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.Specs[name]
	return s, ok
}

// FailWith makes the host fail what it's asked to do, with err (nil puts it
// right).
func (h *FakeHost) FailWith(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Fail = err
}

// ErrDiskFull is a host failure, for FailWith.
var ErrDiskFull = errors.New("no space left on device")

// Home is a browser on the base domain.
func (w *World) Home() *Browser {
	return &Browser{T: w.T, W: w, Host: BaseDomain, Cookies: map[string]*http.Cookie{}}
}

// Handoff follows a console handoff link (from the home page's Open) as
// its browser: the page that asks who it opens the console as, then its
// button. What the button gives back, or the link's own answer if it
// doesn't ask.
func (b *Browser) Handoff(link string) Response {
	b.T.Helper()
	r := b.Get(link)
	if r.Status != http.StatusOK || !strings.Contains(r.Body, `action="/welcome"`) {
		return r
	}
	u, _ := url.Parse(link)
	return b.Post("/welcome", url.Values{"code": {u.Query().Get("code")}})
}

// SignInWithGoogle goes through Google sign-in as email.
func (b *Browser) SignInWithGoogle(email string) Response {
	b.T.Helper()
	start := b.Get("/signin/google")
	if start.Status != http.StatusSeeOther {
		b.T.Fatalf("starting Google sign-in: %d %s", start.Status, start.Body)
	}
	back := b.W.Google.Approve(start.Header.Get("Location"), email, "sub-"+email)
	u, _ := url.Parse(back)
	if u.Host != b.Host {
		b.T.Fatalf("Google sends people back to %s, not %s", u.Host, b.Host)
	}
	return b.Get(u.RequestURI())
}

// OnHome signs in with Google as email, let in first (Setup.Admit).
func (w *World) OnHome(email string) *Browser {
	w.T.Helper()
	if w.admit != nil {
		w.admit(email)
	}
	b := w.Home()
	if r := b.SignInWithGoogle(email); r.Status != http.StatusSeeOther {
		w.T.Fatalf("signing in as %s: %d %s", email, r.Status, r.Body)
	}
	return b
}

// CreateForm is a good create-a-network form for ada.
func CreateForm(name string) url.Values {
	return url.Values{"name": {name}, "template": {"team"}, "timezone": {"Europe/Paris"}, "nick": {"ada"},
		"password": {"correct-horse-battery"}, "provider": {"anthropic"}, "model_key": {"sk-ant-api03-test-key-0123456789"}}
}

// WaitReady follows a network's status page until it's ready (or fails).
func (b *Browser) WaitReady(name string) Response {
	b.T.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := b.Get("/networks/" + name)
		if r.Status != http.StatusOK || !strings.Contains(r.Text(), "Setting up") || time.Now().After(deadline) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// templatesDir is where new networks' templates are, found from this file
// (the tests that use it run in their own packages' directories).
func templatesDir() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "templates")
}
