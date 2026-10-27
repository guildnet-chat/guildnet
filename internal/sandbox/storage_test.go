// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// A network's desks' files, in all, are limited to its storage: the
// platform puts it in the network's token (its plan's), and sandboxd sets it
// before the network's desks are used. A token without one (a self-hosted
// console's) gets sandboxd's default, for a network not yet limited.

// sizedBackend is a backend that can limit a network's files.
type sizedBackend struct {
	*fakeBackend
	mu  sync.Mutex
	set []string // "network GB", in order
}

func (b *sizedBackend) SetStorage(_ context.Context, network string, gb int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.set = append(b.set, fmt.Sprintf("%s %d", network, gb))
	return nil
}

// Used is each of network's desks' files, as the fake has them: a byte a
// byte of content.
func (b *sizedBackend) Used(_ context.Context, network string) (map[string]int64, error) {
	b.fakeBackend.mu.Lock()
	defer b.fakeBackend.mu.Unlock()
	out := map[string]int64{}
	for k, content := range b.fakeBackend.files {
		if desk, ok := strings.CutPrefix(k, network+"/"); ok {
			nick, _, _ := strings.Cut(desk, " ")
			out[nick] += int64(len(content))
		}
	}
	return out, nil
}

func (b *sizedBackend) limits() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.set)
}

func startSized(t *testing.T, defaultGB int) (*rig, *sizedBackend) {
	t.Helper()
	b := &sizedBackend{fakeBackend: newBackend()}
	c := &clock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	s := sandbox.NewServer(sandbox.Options{Backend: b, PublicKeys: []ed25519.PublicKey{publicKey}, Now: c.Now, DefaultStorageGB: defaultGB})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &rig{backend: b.fakeBackend, server: s, url: srv.URL, clock: c}, b
}

func (r *rig) clientWith(network string, gb int) *desk.Remote {
	return desk.NewRemote(r.url, sandbox.DesksTokenFor(privateKey, network, gb, r.clock.Now(), 15*time.Minute))
}

func TestANetworksStorageIsSetFromItsToken(t *testing.T) {
	r, b := startSized(t, 5)
	ctx := context.Background()

	r.clientWith("acme", 20).Open(ctx, "kai")
	r.clientWith("acme", 20).Write(ctx, "kai", "/workspace/a.txt", []byte("x"))
	r.clientWith("acme", 20).Open(ctx, "ada")
	r.clientWith("acme", 100).Open(ctx, "kai") // a bigger plan

	if got, want := b.limits(), []string{"acme 20", "acme 100"}; !slices.Equal(got, want) {
		t.Errorf("limits set %q, want %q: once, and again when it changes", got, want)
	}
}

func TestATokenWithoutStorageGetsTheDefaultOnce(t *testing.T) {
	r, b := startSized(t, 5)
	ctx := context.Background()

	r.clientWith("acme", 20).Open(ctx, "kai")
	r.client("acme").Open(ctx, "kai") // says nothing: acme's limit stays
	r.client("globex").Open(ctx, "kai")
	r.client("globex").Open(ctx, "kai")

	if got, want := b.limits(), []string{"acme 20", "globex 5"}; !slices.Equal(got, want) {
		t.Errorf("limits set %q, want %q", got, want)
	}
}

func TestWithNoDefaultATokenWithoutStorageSetsNoLimit(t *testing.T) {
	r, b := startSized(t, 0)

	if err := r.client("acme").Open(context.Background(), "kai"); err != nil {
		t.Fatal(err)
	}

	if got := b.limits(); len(got) != 0 {
		t.Errorf("limits set %q, want none", got)
	}
}

// The platform asks how much of its storage a network's desks use, and
// each desk's share; only the platform: a runner's token doesn't say.
func TestThePlatformSeesANetworksStorageUse(t *testing.T) {
	r, _ := startSized(t, 5)
	ctx := context.Background()
	r.clientWith("acme", 20).Write(ctx, "kai", "/workspace/a.txt", []byte("12345"))
	r.clientWith("acme", 20).Write(ctx, "vera", "/workspace/b.txt", []byte("123"))
	r.clientWith("globex", 20).Write(ctx, "hal", "/workspace/c.txt", []byte("1234567"))

	got, err := r.admin("acme").Storage(ctx)

	if err != nil {
		t.Fatal(err)
	}
	if got.LimitGB != 20 || got.Used() != 8 || got.Desks["kai"] != 5 || got.Desks["vera"] != 3 || len(got.Desks) != 2 {
		t.Errorf("acme's storage = %+v (used %d), want 20 GB, kai 5 and vera 3", got, got.Used())
	}
	if _, err := r.clientWith("acme", 20).Storage(ctx); err == nil {
		t.Error("a runner's token was told its network's storage")
	}
}

// Before a network's desks are first used, its limit is sandboxd's default.
func TestAnUnusedNetworksStorageIsTheDefault(t *testing.T) {
	r, _ := startSized(t, 5)

	got, err := r.admin("acme").Storage(context.Background())

	if err != nil || got.LimitGB != 5 || got.Used() != 0 {
		t.Errorf("acme's storage = %+v %v, want 5 GB, none used", got, err)
	}
}

// A command can be given more input than a file write takes, as a stream:
// the runner pipes a repository's pack into git at the desk that way.
func TestALargeInputIsPipedToACommand(t *testing.T) {
	r := start(t)
	big := bytes.Repeat([]byte("x"), 20<<20)

	out, err := r.client("acme").Pipe(context.Background(), "kai", desk.Command{Args: []string{"git", "index-pack", "--stdin"}, Dir: "/workspace/web", Timeout: time.Minute}, bytes.NewReader(big))

	if err != nil {
		t.Fatal(err)
	}
	// The stand-in prints what came in: the first 64 KiB, then how much more.
	if want := fmt.Sprintf("%d more bytes", len(big)-64<<10); !strings.Contains(out.Text, want) {
		t.Errorf("the command didn't get all 20 MiB: %.100q...%q", out.Text, out.Text[max(0, len(out.Text)-120):])
	}
	e := r.backend.execs[len(r.backend.execs)-1]
	if strings.Join(e.Args, " ") != "git index-pack --stdin" || e.Dir != "/workspace/web" {
		t.Errorf("ran %v in %s", e.Args, e.Dir)
	}
}

// A runner can't give its network more storage: its token is the
// platform's, and a storage it made up isn't signed.
func TestStorageOnlyComesFromAToken(t *testing.T) {
	r, b := startSized(t, 5)
	_, forger := sandbox.NewKeys()
	forged := desk.NewRemote(r.url, sandbox.DesksTokenFor(forger, "acme", 300, r.clock.Now(), 15*time.Minute))

	if err := forged.Open(context.Background(), "kai"); err == nil {
		t.Error("a token the platform didn't sign opened a desk")
	}
	if got := b.limits(); len(got) != 0 {
		t.Errorf("limits set %q", got)
	}
}
