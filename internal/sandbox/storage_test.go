// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http/httptest"
	"slices"
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
