// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// BrokerCall calls the console's broker as network's runner does, with
// secret, and gives back the status and the JSON answer.
func BrokerCall(t *testing.T, w *World, network, secret, method, path string) (int, map[string]any) {
	t.Helper()
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	req, _ := http.NewRequest(method, broker.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("GuildNet-Network", network)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// RunnerSecret is the secret network's runner calls the broker with.
func RunnerSecret(w *World, network string) string {
	n, _ := w.Store.Network(context.Background(), network)
	return n.RunnerToken
}

var _, SandboxKey = sandbox.NewKeys()

// Set puts the clock at t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// DeskCall is the network's runner calling the broker about desks.
func DeskCall(t *testing.T, w *World, network, secret, path string, body any) (int, map[string]any) {
	t.Helper()
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", broker.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("GuildNet-Network", network)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// DeskStorage is the storage, in GB, sandboxd limits network's desks'
// files to when its runner opens a desk with the token the broker gives it
// (0 for none set).
func DeskStorage(t *testing.T, w *World, network string) int {
	t.Helper()
	status, out := BrokerCall(t, w, network, RunnerSecret(w, network), "GET", "/sandbox")
	if status != http.StatusOK {
		t.Fatalf("the broker's desks token for %s: %d %v", network, status, out)
	}
	vm := &storageVM{}
	sd := httptest.NewServer(sandbox.NewServer(sandbox.Options{Backend: vm, PublicKeys: []ed25519.PublicKey{SandboxKey.Public().(ed25519.PublicKey)}}))
	defer sd.Close()
	token, _ := out["token"].(string)
	req, _ := http.NewRequest("POST", sd.URL+"/v1/desks/kai/open", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("opening a desk with %s's token: %d", network, resp.StatusCode)
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.gb
}

// storageVM is sandboxd's backend, keeping only the storage it's told.
type storageVM struct {
	mu sync.Mutex
	gb int
}

func (v *storageVM) SetStorage(_ context.Context, _ string, gb int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.gb = gb
	return nil
}
func (v *storageVM) Ensure(context.Context, sandbox.Desk) error { return nil }
func (v *storageVM) Exec(context.Context, sandbox.Desk, sandbox.Exec) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (v *storageVM) ReadFile(context.Context, sandbox.Desk, string, int64) ([]byte, error) {
	return nil, sandbox.ErrNoFile
}
func (v *storageVM) WriteFile(context.Context, sandbox.Desk, string, []byte) error { return nil }
func (v *storageVM) Stop(context.Context, sandbox.Desk) error                      { return nil }
func (v *storageVM) Remove(context.Context, sandbox.Desk) error                    { return nil }
func (v *storageVM) RemoveNetwork(context.Context, string) error                   { return nil }

// StartDesk is an agent's desk starting to work: the lease's ID, or "" with
// why not.
func StartDesk(t *testing.T, w *World, network, agent string, resume bool) (id string, status int, out map[string]any) {
	t.Helper()
	status, out = DeskCall(t, w, network, RunnerSecret(w, network), "/desks/start", map[string]any{"agent": agent, "resume": resume})
	id, _ = out["id"].(string)
	return id, status, out
}
