// SPDX-License-Identifier: AGPL-3.0-only

package config_test

import (
	"testing"

	"github.com/guildnet-chat/guildnet/config"
)

// The sample network at the top of the repo (guildnet.yaml and personas/)
// is what `go run ./cmd/runner` starts: it loads, with its owner and agents.
func TestTheSampleNetworkLoads(t *testing.T) {
	cfg, personas, err := config.Load("../guildnet.yaml")

	if err != nil {
		t.Fatal(err)
	}
	if cfg.Owner != "ada" || len(personas) != 3 {
		t.Errorf("owner %q, %d agents", cfg.Owner, len(personas))
	}
	for _, p := range personas {
		if p.ReportsTo == "" || p.Prompt == "" {
			t.Errorf("%s has no manager or prompt", p.Nick)
		}
	}
}
