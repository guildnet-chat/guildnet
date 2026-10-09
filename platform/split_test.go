// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The console doesn't need a network's runner, not even through another
// package: it only ever calls the runner's API.
func TestTheConsoleDoesntNeedTheRunner(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/guildnet-chat/guildnet/platform").CombinedOutput()
	if err != nil {
		t.Skipf("go list: %v: %s", err, out)
	}
	if slices.Contains(strings.Fields(string(out)), "github.com/guildnet-chat/guildnet/runner") {
		t.Error("the console depends on the runner")
	}
}
