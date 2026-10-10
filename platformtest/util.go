// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"strconv"
	"testing"
	"time"
)

// Itoa is n in decimal.
func Itoa(n int64) string { return strconv.FormatInt(n, 10) }

// WaitFor waits up to five seconds for cond.
func WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("gave up waiting for %s", what)
}
