// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Desks of GuildNet's own are in sandboxd, on the sandbox VM. A network's
// runner gets from the broker where sandboxd is and its network's token
// (signed with a key only the platform and sandboxd hold), and can ask for
// the VM to be woken when it's asleep.

type waker struct {
	mu    sync.Mutex
	times int
}

func (k *waker) wake() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.times++
	return nil
}

func (k *waker) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.times
}

func sandboxWorld(t *testing.T) (*World, *waker) {
	t.Helper()
	k := &waker{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, AnyoneCreates, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey, Wake: func() error { return k.wake() }}
	})
	return w, k
}

func TestTheBrokerGivesARunnerItsNetworksSandboxToken(t *testing.T) {
	w, _ := sandboxWorld(t)
	w.Created("ada@example.com", "initech")
	w.CreateNetwork("bob@example.com", "hooli")

	status, out := BrokerCall(t, w, "initech", RunnerSecret(w, "initech"), "GET", "/sandbox")

	if status != http.StatusOK || out["url"] != "http://10.20.0.5:8443" {
		t.Fatalf("%d %v", status, out)
	}
	// A token sandboxd takes for initech's desks (not for removing them
	// all), good for a few minutes: it says until when.
	vm := httptest.NewServer(sandbox.NewServer(sandbox.Options{Backend: vmBackend{&desksVM{}}, PublicKeys: []ed25519.PublicKey{SandboxKey.Public().(ed25519.PublicKey)}}))
	defer vm.Close()
	token, _ := out["token"].(string)
	if err := desk.NewRemote(vm.URL, token).Open(context.Background(), "kai"); err != nil {
		t.Errorf("sandboxd refused initech's token: %v", err)
	}
	if err := desk.NewRemote(vm.URL, token).RemoveAll(context.Background()); err == nil {
		t.Error("a runner's token removed every desk")
	}
	if until, err := time.Parse(time.RFC3339, fmt.Sprint(out["expires"])); err != nil || until.Sub(time.Now()) > 16*time.Minute || until.Before(time.Now()) {
		t.Errorf("good until %v (%v), want in a few minutes", out["expires"], err)
	}
	if status, out := BrokerCall(t, w, "initech", RunnerSecret(w, "hooli"), "GET", "/sandbox"); status != http.StatusUnauthorized || out["token"] != nil {
		t.Errorf("with another network's secret: %d %v", status, out)
	}
}

func TestWithoutASandboxTheBrokerSaysSo(t *testing.T) {
	w := NewWorld(t)
	w.Created("ada@example.com", "initech")

	status, out := BrokerCall(t, w, "initech", RunnerSecret(w, "initech"), "GET", "/sandbox")

	if status != http.StatusNotFound || out["token"] != nil {
		t.Errorf("%d %v", status, out)
	}
}

// Waking starts the VM; many runners asking at once start it once.
func TestARunnerCanWakeTheSandbox(t *testing.T) {
	w, k := sandboxWorld(t)
	w.Created("ada@example.com", "initech")

	first, _ := BrokerCall(t, w, "initech", RunnerSecret(w, "initech"), "POST", "/sandbox/wake")
	again, _ := BrokerCall(t, w, "initech", RunnerSecret(w, "initech"), "POST", "/sandbox/wake")
	stranger, _ := BrokerCall(t, w, "initech", "not-the-secret", "POST", "/sandbox/wake")

	if first != http.StatusAccepted || again != http.StatusAccepted || stranger != http.StatusUnauthorized {
		t.Errorf("statuses %d, %d, %d", first, again, stranger)
	}
	if n := k.count(); n != 1 {
		t.Errorf("woken %d times", n)
	}
}
