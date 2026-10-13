// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network's desks' files are limited to its storage (its plan's, from
// the host's desk policy): the broker puts it in the network's desks token,
// which sandboxd limits the network's files to.

func storageWorld(t *testing.T, policy *fakeDesks) *World {
	t.Helper()
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, withDeskPolicy(policy), func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey}
	})
}

func TestADesksTokenCarriesTheNetworksStorage(t *testing.T) {
	w := storageWorld(t, &fakeDesks{storage: 20})
	w.Created("ada@example.com", "initech")

	if gb := DeskStorage(t, w, "initech"); gb != 20 {
		t.Errorf("sandboxd set %d GB, want initech's 20", gb)
	}
}

// With no desk policy (a console of one's own), the token says nothing:
// sandboxd's default, if any, applies.
func TestWithoutADeskPolicyTheTokenLeavesStorageToSandboxd(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, AnyoneCreates, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey}
	})
	w.Created("ada@example.com", "initech")

	if gb := DeskStorage(t, w, "initech"); gb != 0 {
		t.Errorf("sandboxd set %d GB, want none", gb)
	}
}

// Without the network's storage, no token: one saying nothing would leave
// a network new to sandboxd at its default, smaller than its plan's.
func TestWithoutItsStorageTheBrokerSaysToTryAgain(t *testing.T) {
	w := storageWorld(t, &fakeDesks{storage: 20, err: errors.New("connection refused")})
	w.Created("ada@example.com", "initech")

	status, out := BrokerCall(t, w, "initech", RunnerSecret(w, "initech"), "GET", "/sandbox")

	if status != http.StatusServiceUnavailable || out["token"] != nil {
		t.Errorf("%d %v, want 503 and no token", status, out)
	}
}
