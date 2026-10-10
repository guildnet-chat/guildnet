// SPDX-License-Identifier: AGPL-3.0-only

package app

import "testing"

// A network's connectors' tokens come from the platform's broker, which the
// runner is pointed at for its model keys or, when the network keeps its
// keys itself (as one run by its owner can), for GitHub.
func TestConnectorsUseWhicheverBrokerTheRunnerHas(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"keys and GitHub from the broker", map[string]string{"GUILDNET_KEYS_BROKER": "https://broker:9443", "GUILDNET_GITHUB_BROKER": "https://broker:9443"}, "https://broker:9443"},
		{"its own keys, GitHub from the broker", map[string]string{"GUILDNET_GITHUB_BROKER": "http://platform:9090"}, "http://platform:9090"},
		{"no broker", map[string]string{}, ""},
	} {
		if got := connectorBroker(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
