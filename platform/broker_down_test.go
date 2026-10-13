// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// While the platform's database is out of reach (it's being upgraded, say),
// the broker can't check a runner's secret: it says to try again, not that
// the secret is wrong, which would look like the runner's fault.

// downStore is a store whose networks can't be looked up while it's down.
type downStore struct {
	Store
	down atomic.Bool
}

func (s *downStore) Network(ctx context.Context, name string) (platform.Network, error) {
	if s.down.Load() {
		return platform.Network{}, errors.New("dial tcp 10.200.2.2:5432: connect: connection refused")
	}
	return s.Store.Network(ctx, name)
}

func TestWhileTheDatabaseIsDownTheBrokerSaysToTryAgain(t *testing.T) {
	s := &downStore{Store: platform.NewMemoryStore()}
	w := NewWorldWith(t, func() Store { return s }, AnyoneCreates)
	w.Created("ada@example.com", "initech")
	secret := RunnerSecret(w, "initech")
	s.down.Store(true)

	for _, call := range []struct{ method, path string }{
		{"GET", "/model-keys"}, {"GET", "/connectors"}, {"GET", "/git"},
		{"GET", "/sandbox"}, {"GET", "/settings"}, {"GET", "/two-step"},
	} {
		if status, out := BrokerCall(t, w, "initech", secret, call.method, call.path); status != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d %v, want 503", call.method, call.path, status, out)
		}
	}
}

func TestAWrongSecretIsStillRefused(t *testing.T) {
	s := &downStore{Store: platform.NewMemoryStore()}
	w := NewWorldWith(t, func() Store { return s }, AnyoneCreates)
	w.Created("ada@example.com", "initech")

	for _, network := range []string{"initech", "nobody"} {
		if status, out := BrokerCall(t, w, network, "not-the-secret", "GET", "/model-keys"); status != http.StatusUnauthorized {
			t.Errorf("%s: %d %v, want 401", network, status, out)
		}
	}
}
