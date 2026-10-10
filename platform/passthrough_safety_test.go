// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"fmt"
	"net/http"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The agent API passes calls on under /agents/v1 only: a path that climbs
// out of it with ".." reaches nothing else of the runner's.
func TestAnAgentsCallCantClimbOutOfTheAgentAPI(t *testing.T) {
	w := NewWorld(t)
	_, _, token := HireOutside(t, w)

	for _, path := range []string{"/agents/v1/../../v1/feedback", "/agents/v1/%2e%2e/%2e%2e/v1/stats", "/models/v1/../../v1/feedback"} {
		r := w.Program("acme", token, "GET", path, nil)
		if r.Status == http.StatusOK || r.Status/100 == 3 {
			t.Errorf("%s: %d %s", path, r.Status, r.Body)
		}
	}
}

// Made-up tokens each start with a fresh allowance, so the network as a
// whole has one too: a flood of them can't pass through to its runner.
func TestMadeUpTokensCantFloodANetwork(t *testing.T) {
	w := NewWorld(t)
	var last Response
	for i := range 700 {
		last = w.Program("acme", fmt.Sprintf("gna_made-up-%04d-0123456789abcdef", i), "GET", "/agents/v1/me", nil)
		if last.Status == http.StatusTooManyRequests {
			break
		}
	}
	if last.Status != http.StatusTooManyRequests {
		t.Errorf("700 calls with made-up tokens in a minute: the last was %d", last.Status)
	}
}
