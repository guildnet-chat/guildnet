// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// StartDesk is an agent's desk starting to work: the lease's ID, or "" with
// why not.
func StartDesk(t *testing.T, w *World, network, agent string, resume bool) (id string, status int, out map[string]any) {
	t.Helper()
	status, out = DeskCall(t, w, network, RunnerSecret(w, network), "/desks/start", map[string]any{"agent": agent, "resume": resume})
	id, _ = out["id"].(string)
	return id, status, out
}
