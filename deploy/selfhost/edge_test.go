// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
	"regexp"
	"testing"
)

// The edge has no admin API. In one pod or task (GKE, Fargate) every
// container shares localhost, so Caddy's API on :2019 would let any of
// them change where the edge sends visitors. Its config changes only with
// a restart.
func TestTheEdgeHasNoAdminAPI(t *testing.T) {
	b, err := os.ReadFile("../caddy/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if !adminOff(string(b)) {
		t.Error("the Caddyfile's global options don't say admin off")
	}
}

// adminOff reports whether a Caddyfile's global options (its first block,
// with no address) turn the admin API off.
func adminOff(caddyfile string) bool {
	global := regexp.MustCompile(`(?ms)^\{\n(.*?)^\}`).FindStringSubmatch(caddyfile)
	return global != nil && regexp.MustCompile(`(?m)^\s*admin\s+off\s*$`).MatchString(global[1])
}
