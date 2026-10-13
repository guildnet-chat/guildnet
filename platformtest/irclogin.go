// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BrowserAddress is where a Browser's requests come from, as the console
// sees them.
const BrowserAddress = "127.0.0.1"

// IRCLogin is network's Ergo asking the console about a login, as
// guildnet-ircauth does: "yes", "no" or "not-mine".
func IRCLogin(t *testing.T, w *World, network, account, password, ip string) string {
	t.Helper()
	n, err := w.Store.Network(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	status, out := IRCLoginAs(t, w, network, n.ErgoToken, account, password, ip)
	if status != http.StatusOK {
		t.Fatalf("the console answered %s's login with %d %v", account, status, out)
	}
	answer, _ := out["answer"].(string)
	return answer
}

// IRCLoginAs is IRCLogin with the Ergo token given.
func IRCLoginAs(t *testing.T, w *World, network, token, account, password, ip string) (int, map[string]any) {
	t.Helper()
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	body, _ := json.Marshal(map[string]string{"account": account, "password": password, "ip": ip})
	req, _ := http.NewRequest("POST", broker.URL+"/irc/login", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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

// SignsInOverIRC says whether account signs in over network's IRC with
// password: the console says yes when Ergo asks.
func SignsInOverIRC(t *testing.T, w *World, network, account, password string) bool {
	t.Helper()
	return IRCLogin(t, w, network, account, password, "198.51.100.200") == "yes"
}
