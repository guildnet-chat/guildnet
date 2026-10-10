// SPDX-License-Identifier: AGPL-3.0-only

// Package netspec is what's said about a network between the console and
// whatever runs it: what the console asks for (Spec), where it then reaches
// the network (Addresses), how much of the machine networks use (Stats), and
// which names a network can have. It's shared, so the console doesn't need
// the service that runs networks on GuildNet's machines (package host).
package netspec

import (
	"regexp"

	"github.com/guildnet-chat/guildnet/models"
)

// Spec is what the platform asks for: a network's template, its owner and
// settings, and the secrets the platform itself uses to reach it.
type Spec struct {
	Template  string `json:"template"`
	Owner     string `json:"owner"` // the owner's IRC nick
	Company   string `json:"company"`
	Timezone  string `json:"timezone"`
	WebOrigin string `json:"web_origin"` // where its web chat is served, e.g. https://acme.guildnet.chat
	// The platform's secrets for this network: its runner's API, its Ergo's
	// API, and the key that signs web chat sign-ins.
	RunnerToken string `json:"runner_token"`
	ErgoToken   string `json:"ergo_token"`
	ChatKey     string `json:"chat_key"`
	// ModelKeys are the owner's keys for the model providers its agents run
	// on, by provider. AnthropicKey is how the platform sent the one key
	// there used to be; it counts as the Anthropic key.
	ModelKeys    map[models.Provider]string `json:"model_keys,omitempty"`
	AnthropicKey string                     `json:"anthropic_key,omitempty"`
}

// Keys are s's model providers' keys, the old way's included.
func (s Spec) Keys() map[models.Provider]string {
	out := map[models.Provider]string{}
	for p, k := range s.ModelKeys {
		out[p] = k
	}
	if s.AnthropicKey != "" && out[models.Anthropic] == "" {
		out[models.Anthropic] = s.AnthropicKey
	}
	return out
}

// Addresses are where the platform reaches a network, from the private
// network it's joined to.
type Addresses struct {
	RunnerURL string `json:"runner_url"`
	ErgoURL   string `json:"ergo_url"`
	ChatURL   string `json:"chat_url"`
}

// Stats are how much of the machine the networks use.
type Stats struct {
	MemoryTotal     int64                   `json:"memory_total"` // bytes; 0 if unknown
	MemoryAvailable int64                   `json:"memory_available"`
	Networks        map[string]NetworkStats `json:"networks"`
}

// NetworkStats are one network's containers, added up.
type NetworkStats struct {
	Running     bool    `json:"running"` // all of them are
	Memory      int64   `json:"memory"`
	MemoryLimit int64   `json:"memory_limit"`
	CPU         float64 `json:"cpu"`
}

var networkName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidName says whether name can be a network's name: lowercase letters,
// digits, and single hyphens between them, 2 to 30 characters, starting
// with a letter. It becomes a host name, a Docker name, and a database name.
func ValidName(name string) bool {
	return len(name) >= 2 && len(name) <= 30 && networkName.MatchString(name)
}
