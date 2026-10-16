// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// sandboxd keeps agents' desks: a runner reaches its network's desks with
// the token the platform signed for that network, and nobody else's. Here
// the runner's own client talks to the real server, over HTTP, with Docker
// replaced by a stand-in.

type fakeBackend struct {
	mu       sync.Mutex
	files    map[string]string // "network/nick path"
	running  map[string]bool
	ensured  []string
	stopped  []string
	removed  []string
	execs    []sandbox.Exec
	busy     chan struct{} // when set, exec waits for it to close
	printing string        // what exec prints
	exit     int
}

func newBackend() *fakeBackend {
	return &fakeBackend{files: map[string]string{}, running: map[string]bool{}}
}

func key(d sandbox.Desk) string { return d.Network + "/" + d.Nick }

func (f *fakeBackend) Ensure(_ context.Context, d sandbox.Desk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, key(d))
	f.running[key(d)] = true
	return nil
}

func (f *fakeBackend) Exec(ctx context.Context, d sandbox.Desk, e sandbox.Exec) (sandbox.Result, error) {
	f.mu.Lock()
	f.execs = append(f.execs, e)
	busy, printing, exit := f.busy, f.printing, f.exit
	f.mu.Unlock()
	if busy != nil {
		<-busy
	}
	if e.Stdin != nil {
		in, _ := io.ReadAll(e.Stdin)
		printing += string(in)
	}
	io.WriteString(e.Output, printing)
	return sandbox.Result{Exit: exit}, nil
}

func (f *fakeBackend) ReadFile(_ context.Context, d sandbox.Desk, path string, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.files[key(d)+" "+path]
	if !ok {
		return nil, sandbox.ErrNoFile
	}
	if int64(len(s)) > max {
		return nil, sandbox.ErrTooBig
	}
	return []byte(s), nil
}

func (f *fakeBackend) WriteFile(_ context.Context, d sandbox.Desk, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[key(d)+" "+path] = string(data)
	return nil
}

func (f *fakeBackend) Stop(_ context.Context, d sandbox.Desk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, key(d))
	f.running[key(d)] = false
	return nil
}

func (f *fakeBackend) Remove(_ context.Context, d sandbox.Desk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, key(d))
	return nil
}

func (f *fakeBackend) RemoveNetwork(_ context.Context, network string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, network+"/*")
	for k := range f.files {
		if strings.HasPrefix(k, network+"/") {
			delete(f.files, k)
		}
	}
	return nil
}

func (f *fakeBackend) lastExec() sandbox.Exec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs[len(f.execs)-1]
}

// The platform signs networks' tokens with its private key; sandboxd holds
// only the public one.
var publicKey, privateKey = sandbox.NewKeys()

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type rig struct {
	backend *fakeBackend
	server  *sandbox.Server
	url     string
	clock   *clock
}

func start(t *testing.T) *rig {
	t.Helper()
	b := newBackend()
	c := &clock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	s := sandbox.NewServer(sandbox.Options{Backend: b, PublicKeys: []ed25519.PublicKey{publicKey}, Now: c.Now})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &rig{backend: b, server: s, url: srv.URL, clock: c}
}

// startHolding is a sandbox with room for max desks running at once.
func startHolding(t *testing.T, max int) *rig {
	t.Helper()
	b := newBackend()
	c := &clock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	s := sandbox.NewServer(sandbox.Options{Backend: b, PublicKeys: []ed25519.PublicKey{publicKey}, Now: c.Now, MaxRunning: max})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &rig{backend: b, server: s, url: srv.URL, clock: c}
}

// client is a network's runner, with a desks token the platform signed now.
func (r *rig) client(network string) *desk.Remote {
	return desk.NewRemote(r.url, sandbox.TokenFor(privateKey, network, sandbox.Desks, r.clock.Now(), 15*time.Minute))
}

// admin is the platform, deleting a network's desks.
func (r *rig) admin(network string) *desk.Remote {
	return desk.NewRemote(r.url, sandbox.TokenFor(privateKey, network, sandbox.Admin, r.clock.Now(), 2*time.Minute))
}

// A runner's token is good for a while (a few minutes' slack for clocks),
// then it needs a new one: one that leaked doesn't work for long.
func TestATokenIsGoodForAWhileOnly(t *testing.T) {
	r := start(t)
	c := r.client("acme")
	if err := c.Open(context.Background(), "kai"); err != nil {
		t.Fatal(err)
	}

	r.clock.add(17 * time.Minute)

	if err := c.Open(context.Background(), "kai"); err == nil {
		t.Error("a token past its time still worked")
	}
	if err := r.client("acme").Open(context.Background(), "kai"); err != nil {
		t.Errorf("a new one: %v", err)
	}
	long := desk.NewRemote(r.url, sandbox.TokenFor(privateKey, "acme", sandbox.Desks, r.clock.Now(), 48*time.Hour))
	if err := long.Open(context.Background(), "kai"); err == nil {
		t.Error("a token made to last two days worked")
	}
}

// A runner's token works its network's desks; only the platform's removes
// them all.
func TestOnlyThePlatformRemovesANetworksDesks(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	r.client("acme").Write(ctx, "kai", "/workspace/a.txt", []byte("x"))

	if err := r.client("acme").RemoveAll(ctx); err == nil {
		t.Error("a runner's token removed every desk")
	}
	if err := r.admin("acme").Open(ctx, "kai"); err == nil {
		t.Error("the platform's removal token opened a desk")
	}
	if err := r.admin("acme").RemoveAll(ctx); err != nil {
		t.Errorf("the platform's: %v", err)
	}
}

// A network's egress pass (which its desks hold, to reach its git hosts
// through the proxy) does nothing here.
func TestAnEgressPassDoesntWorkDesks(t *testing.T) {
	r := start(t)
	pass := sandbox.PassFor(privateKey, "acme", []string{"git.acme.example"}, r.clock.Now(), 15*time.Minute)

	if err := desk.NewRemote(r.url, pass).Open(context.Background(), "kai"); err == nil {
		t.Error("an egress pass opened a desk")
	}
}

// Once a network's desks are removed (it's deleted), no token made before
// works again: not with a network made later with the same name.
func TestATokenFromBeforeANetworkWasDeletedIsRefused(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	old := r.client("acme")
	r.clock.add(time.Minute)
	if err := r.admin("acme").RemoveAll(ctx); err != nil {
		t.Fatal(err)
	}
	r.clock.add(time.Second)

	if err := old.Open(ctx, "kai"); err == nil {
		t.Error("the deleted network's token opened a desk of the network made after it")
	}
	if err := r.client("acme").Open(ctx, "kai"); err != nil {
		t.Errorf("the new network's own token: %v", err)
	}
}

// Old tokens (the network's name, signed, for ever) open nothing: every
// runner has the new ones, which run out.
func TestOldTokensOpenNothing(t *testing.T) {
	r := start(t)
	sig := ed25519.Sign(privateKey, []byte("guildnet sandbox|acme"))
	old := "acme." + base64.RawURLEncoding.EncodeToString(sig)

	if err := desk.NewRemote(r.url, old).Open(context.Background(), "kai"); err == nil {
		t.Error("an old token opened a desk")
	}
}

// While the key's changed, tokens signed with either key work.
func TestTokensFromEitherKeyWorkWhileTheKeyChanges(t *testing.T) {
	newPublic, newPrivate := sandbox.NewKeys()
	b := newBackend()
	srv := httptest.NewServer(sandbox.NewServer(sandbox.Options{Backend: b, PublicKeys: []ed25519.PublicKey{publicKey, newPublic}}))
	defer srv.Close()
	for _, key := range []ed25519.PrivateKey{privateKey, newPrivate} {
		if err := desk.NewRemote(srv.URL, sandbox.TokenFor(key, "acme", sandbox.Desks, time.Now(), time.Minute)).Open(context.Background(), "kai"); err != nil {
			t.Errorf("a token from one of the keys: %v", err)
		}
	}
}

func TestARunnerWorksInItsAgentsDesk(t *testing.T) {
	r := start(t)
	r.backend.printing = "ok  guildnet\n"
	c := r.client("acme")
	ctx := context.Background()

	if err := c.Open(ctx, "kai"); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, "kai", "/workspace/notes.md", []byte("# plan\n")); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(ctx, "kai", "/workspace/notes.md", 1<<20)
	out, rerr := c.Run(ctx, "kai", desk.Command{Args: []string{"bash", "-lc", "go test ./..."}, Dir: "/workspace", Timeout: time.Minute})

	if err != nil || string(got) != "# plan\n" {
		t.Errorf("read %q, %v", got, err)
	}
	if rerr != nil || out.Text != "ok  guildnet\n" || out.Exit != 0 {
		t.Errorf("run: %+v, %v", out, rerr)
	}
	e := r.backend.lastExec()
	if strings.Join(e.Args, " ") != "bash -lc go test ./..." || e.Dir != "/workspace" || e.Timeout != time.Minute {
		t.Errorf("the backend ran %+v", e)
	}
	if len(r.backend.ensured) != 1 || r.backend.ensured[0] != "acme/kai" {
		t.Errorf("desks made: %v", r.backend.ensured)
	}
}

// The VM has memory for so many desks at once. A desk wanting to start
// makes room by stopping the one left idle longest (its files stay); one
// that's working, or was a moment ago, is never stopped for it.
func TestTheSandboxMakesRoomByStoppingTheLongestIdleDesk(t *testing.T) {
	r := startHolding(t, 2)
	ctx := context.Background()
	r.client("acme").Open(ctx, "kai")
	r.clock.add(time.Minute)
	r.client("globex").Open(ctx, "hal")
	r.clock.add(5 * time.Minute)

	if err := r.client("initech").Open(ctx, "pam"); err != nil {
		t.Fatalf("a third desk, two idle: %v", err)
	}
	if !slices.Equal(r.backend.stopped, []string{"acme/kai"}) {
		t.Errorf("stopped %v, want the longest idle, acme's kai", r.backend.stopped)
	}

	// kai comes back: hal is now the longest idle.
	r.clock.add(3 * time.Minute)
	if err := r.client("acme").Open(ctx, "kai"); err != nil {
		t.Fatalf("kai again: %v", err)
	}
	if !slices.Equal(r.backend.stopped, []string{"acme/kai", "globex/hal"}) {
		t.Errorf("stopped %v", r.backend.stopped)
	}
}

func TestWhenEveryDeskIsWorkingTheSandboxIsFull(t *testing.T) {
	r := startHolding(t, 2)
	ctx := context.Background()
	r.client("acme").Open(ctx, "kai")
	r.client("globex").Open(ctx, "hal")
	r.clock.add(30 * time.Second) // used a moment ago: still at work

	err := r.client("initech").Open(ctx, "pam")

	if !errors.Is(err, desk.ErrFull) {
		t.Errorf("a third desk while two work: %v", err)
	}
	if len(r.backend.stopped) != 0 {
		t.Errorf("stopped %v", r.backend.stopped)
	}
	// A desk already running carries on.
	if _, err := r.client("acme").Run(ctx, "kai", desk.Command{Args: []string{"true"}}); err != nil {
		t.Errorf("kai carrying on: %v", err)
	}
}

// One network has only its share of the sandbox's desks at once: the rest
// stay for other networks. At its share, a network makes room by stopping
// one of its own idle desks, never another's.
func TestOneNetworkHasOnlyItsShareOfTheDesks(t *testing.T) {
	b := newBackend()
	c := &clock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	s := sandbox.NewServer(sandbox.Options{Backend: b, PublicKeys: []ed25519.PublicKey{publicKey}, Now: c.Now, MaxRunning: 3, MaxPerNetwork: 2})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	r := &rig{backend: b, server: s, url: srv.URL, clock: c}
	ctx := context.Background()
	r.client("acme").Open(ctx, "kai")
	r.client("acme").Open(ctx, "vera")
	c.add(30 * time.Second) // used a moment ago: still at work

	if err := r.client("acme").Open(ctx, "sam"); !errors.Is(err, desk.ErrFull) {
		t.Errorf("acme's third desk while its two work: %v", err)
	}
	if err := r.client("globex").Open(ctx, "hal"); err != nil {
		t.Errorf("globex's desk, with room left: %v", err)
	}

	c.add(time.Hour) // all idle now
	if err := r.client("acme").Open(ctx, "sam"); err != nil {
		t.Fatalf("acme's third desk, its others idle: %v", err)
	}
	for _, d := range b.stopped {
		if !strings.HasPrefix(d, "acme/") {
			t.Errorf("stopped %s to make room for acme", d)
		}
	}
	if len(b.stopped) != 1 {
		t.Errorf("stopped %v, want one of acme's", b.stopped)
	}
}

func TestADeskRunningACommandIsNeverStopped(t *testing.T) {
	r := startHolding(t, 1)
	ctx := context.Background()
	r.backend.busy = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.client("acme").Run(ctx, "kai", desk.Command{Args: []string{"make"}, Timeout: time.Hour})
		done <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.backend.mu.Lock()
		n := len(r.backend.execs)
		r.backend.mu.Unlock()
		if n > 0 {
			break
		}
	}
	r.clock.add(time.Hour)

	err := r.client("globex").Open(ctx, "hal")
	close(r.backend.busy)

	if !errors.Is(err, desk.ErrFull) || len(r.backend.stopped) != 0 {
		t.Errorf("err %v, stopped %v", err, r.backend.stopped)
	}
	if err := <-done; err != nil {
		t.Errorf("kai's command: %v", err)
	}
}

// Once a network is deleted, the platform has every desk of its removed:
// the token's network's, nobody else's.
func TestAllOfANetworksDesksGoTogether(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	r.client("acme").Write(ctx, "kai", "/workspace/a.txt", []byte("acme's"))
	r.client("acme").Write(ctx, "vera", "/workspace/b.txt", []byte("acme's"))
	r.client("globex").Write(ctx, "kai", "/workspace/c.txt", []byte("globex's"))

	if err := r.admin("acme").RemoveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := desk.NewRemote(r.url, "").RemoveAll(ctx); err == nil {
		t.Error("removing with no token worked")
	}

	r.clock.add(time.Second) // a network made after, with the name
	if _, err := r.client("acme").Read(ctx, "kai", "/workspace/a.txt", 1<<20); !errors.Is(err, desk.ErrNoFile) {
		t.Errorf("acme's kai's file after: %v", err)
	}
	if got, err := r.client("globex").Read(ctx, "kai", "/workspace/c.txt", 1<<20); err != nil || string(got) != "globex's" {
		t.Errorf("globex's file after: %q %v", got, err)
	}
}

func TestACallWithoutTheNetworksTokenIsRefused(t *testing.T) {
	r := start(t)
	_, otherKey := sandbox.NewKeys()
	forged := desk.NewRemote(r.url, sandbox.TokenFor(otherKey, "acme", sandbox.Desks, r.clock.Now(), time.Minute))
	none := desk.NewRemote(r.url, "")

	err1 := forged.Open(context.Background(), "kai")
	err2 := none.Open(context.Background(), "kai")

	if err1 == nil || err2 == nil {
		t.Errorf("forged: %v, none: %v; want both refused", err1, err2)
	}
	if len(r.backend.ensured) != 0 {
		t.Errorf("desks made anyway: %v", r.backend.ensured)
	}
}

// Two networks can each have an agent called kai; their desks are apart.
func TestANetworkOnlyReachesItsOwnDesks(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	r.client("globex").Write(ctx, "kai", "/workspace/secret.txt", []byte("globex's"))

	_, err := r.client("acme").Read(ctx, "kai", "/workspace/secret.txt", 1<<20)

	if !errors.Is(err, desk.ErrNoFile) {
		t.Errorf("acme read globex's file: %v", err)
	}
}

// A desk's container is named by its network and nick, so no two desks,
// on any networks, share one.
func TestNoTwoDesksShareAContainer(t *testing.T) {
	names := map[string]sandbox.Desk{}
	for _, d := range []sandbox.Desk{{"acme", "kai"}, {"globex", "kai"}, {"a-b", "c"}, {"a", "b-c"}, {"a_b", "c"}, {"a", "b_c"}} {
		if other, ok := names[d.Name()]; ok {
			t.Errorf("%v and %v are both %s", d, other, d.Name())
		}
		names[d.Name()] = d
	}
}

func TestMissingAndOversizedFilesAreToldApart(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	c := r.client("acme")
	c.Write(ctx, "kai", "/workspace/big.bin", bytes.Repeat([]byte("x"), 100))

	_, missing := c.Read(ctx, "kai", "/workspace/nope", 1<<20)
	_, big := c.Read(ctx, "kai", "/workspace/big.bin", 10)

	if !errors.Is(missing, desk.ErrNoFile) || !errors.Is(big, desk.ErrTooBig) {
		t.Errorf("missing: %v; too big: %v", missing, big)
	}
}

func TestCommandsAreHeldToTenMinutes(t *testing.T) {
	r := start(t)

	r.client("acme").Run(context.Background(), "kai", desk.Command{Args: []string{"make"}, Timeout: 3 * time.Hour})

	if got := r.backend.lastExec().Timeout; got != 10*time.Minute {
		t.Errorf("timeout %v", got)
	}
}

func TestACommandsOutputIsCutShort(t *testing.T) {
	r := start(t)
	r.backend.printing = strings.Repeat("y", 1<<20)

	out, err := r.client("acme").Run(context.Background(), "kai", desk.Command{Args: []string{"yes"}, Timeout: time.Second})

	if err != nil || len(out.Text) > 70<<10 || !strings.Contains(out.Text, "cut short") {
		t.Errorf("%d bytes, %v", len(out.Text), err)
	}
}

func TestACommandsInputReachesIt(t *testing.T) {
	r := start(t)

	out, _ := r.client("acme").Run(context.Background(), "kai", desk.Command{Args: []string{"cat"}, Stdin: "ghs_token", Timeout: time.Second})

	if out.Text != "ghs_token" {
		t.Errorf("got %q", out.Text)
	}
}

func TestAnythingButANickIsRefused(t *testing.T) {
	r := start(t)
	c := r.client("acme")

	for _, nick := range []string{"../etc", "kai/../x", "a b", "", strings.Repeat("k", 40)} {
		if err := c.Open(context.Background(), nick); err == nil {
			t.Errorf("%q was taken as a nick", nick)
		}
	}
}

func TestPathsMustBeAbsolute(t *testing.T) {
	r := start(t)

	err := r.client("acme").Write(context.Background(), "kai", "notes.md", []byte("x"))

	if err == nil {
		t.Error("a relative path was taken")
	}
}

// A desk nobody has used for 10 minutes is stopped to free the VM; its
// files stay. A desk still running a command isn't stopped.
func TestIdleDesksAreStopped(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	c := r.client("acme")
	c.Open(ctx, "kai")
	c.Open(ctx, "pax")
	c.Open(ctx, "vera")
	r.backend.busy = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, "vera", desk.Command{Args: []string{"sleep", "3600"}, Timeout: 10 * time.Minute})
	}()
	waitUntil(t, func() bool { r.backend.mu.Lock(); defer r.backend.mu.Unlock(); return len(r.backend.execs) == 1 })

	r.clock.add(9 * time.Minute)
	c.Open(ctx, "pax")
	r.clock.add(2 * time.Minute)
	r.server.StopIdle(ctx)

	r.backend.mu.Lock()
	stopped := fmt.Sprint(r.backend.stopped)
	r.backend.mu.Unlock()
	if stopped != "[acme/kai]" {
		t.Errorf("stopped %s, want only kai's (pax was used 2 minutes ago, vera is busy)", stopped)
	}
	close(r.backend.busy)
	<-done
}

// Whether any desk is working, and since when all have been idle, so the
// VM can power itself off.
func TestSandboxdSaysHowLongItsBeenIdle(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	r.client("acme").Open(ctx, "kai")

	r.clock.add(7 * time.Minute)

	if idle := r.server.Idle(); idle != 7*time.Minute {
		t.Errorf("idle for %v", idle)
	}
}

// A desk's container can go without sandboxd knowing (Docker restarted, a
// person removed it): each wake-up opens the desk, which checks, so the
// next wake-up heals it.
func TestOpeningADeskAlwaysChecksItsContainer(t *testing.T) {
	r := start(t)
	ctx := context.Background()
	c := r.client("acme")

	c.Open(ctx, "kai")
	c.Open(ctx, "kai")

	if fmt.Sprint(r.backend.ensured) != "[acme/kai acme/kai]" {
		t.Errorf("checked %v, want at each opening", r.backend.ensured)
	}
}

// Keys travel as text (the platform's and sandboxd's settings); the private
// one never goes to the sandbox VM.
func TestKeysWorkWrittenAsText(t *testing.T) {
	public, private := sandbox.NewKeys()
	pubText, privText := sandbox.KeyText(public), sandbox.KeyText(private)

	pub, err1 := sandbox.PublicKeyFrom(pubText)
	priv, err2 := sandbox.PrivateKeyFrom(privText)
	_, wrong := sandbox.PublicKeyFrom(privText)

	if err1 != nil || err2 != nil || wrong == nil {
		t.Fatalf("%v, %v; a private key taken as public: %v", err1, err2, wrong)
	}
	b := newBackend()
	srv := httptest.NewServer(sandbox.NewServer(sandbox.Options{Backend: b, PublicKeys: []ed25519.PublicKey{pub}}))
	defer srv.Close()
	if err := desk.NewRemote(srv.URL, sandbox.TokenFor(priv, "acme", sandbox.Desks, time.Now(), time.Minute)).Open(context.Background(), "kai"); err != nil {
		t.Errorf("a token signed with the key read back was refused: %v", err)
	}
}

// The VM's power-off timer asks, on the VM itself, with no token.
func TestTheIdleTimeCanBeAskedForWithoutAToken(t *testing.T) {
	r := start(t)
	r.client("acme").Open(context.Background(), "kai")
	r.clock.add(90 * time.Second)

	resp, err := http.Get(r.url + "/v1/idle")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Idle int `json:"idle_seconds"`
	}
	json.NewDecoder(resp.Body).Decode(&out)

	if resp.StatusCode != 200 || out.Idle != 90 {
		t.Errorf("%d, idle %d seconds", resp.StatusCode, out.Idle)
	}
}

func TestFiringRemovesTheDesk(t *testing.T) {
	r := start(t)

	if err := r.client("acme").Remove(context.Background(), "kai"); err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(r.backend.removed) != "[acme/kai]" {
		t.Errorf("removed %v", r.backend.removed)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
