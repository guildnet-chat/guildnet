// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A deleted network's desks go with it: their files are its company's, and
// a network made later with the same name mustn't find them.

// desksVM is sandboxd as the platform reaches it, over a stand-in for
// Docker; down, it's asleep (nothing answers).
type desksVM struct {
	srv  *httptest.Server
	mu   sync.Mutex
	down bool
	gone []string // networks whose desks were all removed
	// use is each network's desks' files, in bytes, by nick; clears are the
	// desks removed one at a time ("network/nick").
	use    map[string]map[string]int64
	clears []string
	asked  int // storage asked for, answered or not
}

func newDesksVM(t *testing.T) *desksVM {
	t.Helper()
	vm := &desksVM{use: map[string]map[string]int64{}}
	s := sandbox.NewServer(sandbox.Options{Backend: vmBackend{vm}, PublicKeys: []ed25519.PublicKey{SandboxKey.Public().(ed25519.PublicKey)},
		DefaultStorageGB: 5})
	vm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vm.mu.Lock()
		down := vm.down
		if r.URL.Path == "/v1/storage" {
			vm.asked++
		}
		vm.mu.Unlock()
		if down {
			// Asleep: the connection is dropped, as by a VM that's off.
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
		}
		s.ServeHTTP(w, r)
	}))
	t.Cleanup(vm.srv.Close)
	return vm
}

func (vm *desksVM) setDown(down bool) {
	vm.mu.Lock()
	vm.down = down
	vm.mu.Unlock()
}

func (vm *desksVM) removed(network string) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return slices.Contains(vm.gone, network)
}

func (vm *desksVM) setUse(network string, desks map[string]int64) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.use[network] = desks
}

func (vm *desksVM) storageAsked() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.asked
}

// cleared says whether network's desk nick was removed.
func (vm *desksVM) cleared(network, nick string) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return slices.Contains(vm.clears, network+"/"+nick)
}

// vmBackend is Docker on the VM, as far as removing goes.
type vmBackend struct{ vm *desksVM }

func (b vmBackend) Ensure(context.Context, sandbox.Desk) error { return nil }
func (b vmBackend) Exec(context.Context, sandbox.Desk, sandbox.Exec) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (b vmBackend) ReadFile(context.Context, sandbox.Desk, string, int64) ([]byte, error) {
	return nil, sandbox.ErrNoFile
}
func (b vmBackend) WriteFile(context.Context, sandbox.Desk, string, []byte) error { return nil }
func (b vmBackend) Stop(context.Context, sandbox.Desk) error                      { return nil }
func (b vmBackend) SetStorage(context.Context, string, int) error                 { return nil }
func (b vmBackend) Remove(_ context.Context, d sandbox.Desk) error {
	b.vm.mu.Lock()
	defer b.vm.mu.Unlock()
	b.vm.clears = append(b.vm.clears, d.Network+"/"+d.Nick)
	delete(b.vm.use[d.Network], d.Nick)
	return nil
}
func (b vmBackend) Used(_ context.Context, network string) (map[string]int64, error) {
	b.vm.mu.Lock()
	defer b.vm.mu.Unlock()
	out := map[string]int64{}
	for nick, n := range b.vm.use[network] {
		out[nick] = n
	}
	return out, nil
}
func (b vmBackend) RemoveNetwork(_ context.Context, network string) error {
	b.vm.mu.Lock()
	defer b.vm.mu.Unlock()
	b.vm.gone = append(b.vm.gone, network)
	return nil
}

func desksWorld(t *testing.T) (*World, *desksVM, *waker) {
	t.Helper()
	vm, k := newDesksVM(t), &waker{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, AnyoneCreates, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: vm.srv.URL, Key: SandboxKey, Wake: func() error { return k.wake() }}
	})
	return w, vm, k
}

func TestDeletingANetworkRemovesItsDesks(t *testing.T) {
	w, vm, _ := desksWorld(t)
	_, console := w.Created(ada, "initech")
	console.Get("/settings")
	console.Post("/settings/delete", url.Values{"confirm": {"initech"}})

	w.Host.WaitRemoved(t, "initech")
	for deadline := time.Now().Add(5 * time.Second); !vm.removed("initech") && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if !vm.removed("initech") {
		t.Error("initech's desks weren't removed")
	}
}

// With the sandbox asleep, the name stays taken (the network serves
// nothing meanwhile) until its desks are gone: the VM is woken, and the
// next try removes them.
func TestADeletedNetworksNameWaitsForItsDesksToGo(t *testing.T) {
	w, vm, k := desksWorld(t)
	_, console := w.Created(ada, "initech")
	vm.setDown(true)
	console.Get("/settings")
	console.Post("/settings/delete", url.Values{"confirm": {"initech"}})
	for deadline := time.Now().Add(5 * time.Second); k.count() == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if k.count() == 0 {
		t.Error("the sandbox wasn't woken to remove initech's desks")
	}

	home := w.OnHome("bob@example.com")
	home.Get("/networks/new")
	if r := home.Post("/networks", CreateForm("initech")); r.Status != http.StatusBadRequest || !strings.Contains(r.Text(), "taken") {
		t.Fatalf("taking initech's name while its desks are still there: %d %s", r.Status, r.Text())
	}

	vm.setDown(false)
	home.Get("/networks/new")
	if r := home.Post("/networks", CreateForm("initech")); r.Status != http.StatusSeeOther {
		t.Fatalf("taking the name once the desks can go: %d %s", r.Status, r.Text())
	}
	if !vm.removed("initech") {
		t.Error("the old initech's desks weren't removed before its name was taken")
	}
}
