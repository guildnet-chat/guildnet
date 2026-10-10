// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// The VM's memory decides how many desks run at once: an 8 GB VM holds
// three 2 GB desks, two 3 GB ones; a bigger VM, more.
func TestHowManyDesksFitInTheVM(t *testing.T) {
	meminfo := "MemTotal:        8131360 kB\nMemFree:          200000 kB\n" // a t2d-standard-2
	total, err := sandbox.MemTotal(strings.NewReader(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		desk string
		want int
	}{{"2g", 3}, {"3g", 2}, {"2048m", 3}, {"16g", 1}} {
		size, err := sandbox.ParseMemory(c.desk)
		if err != nil {
			t.Fatal(err)
		}
		if got := sandbox.DesksFor(total, size); got != c.want {
			t.Errorf("%s desks in 8 GB: %d, want %d", c.desk, got, c.want)
		}
	}
	if got := sandbox.DesksFor(32<<30, 2<<30); got != 15 {
		t.Errorf("2g desks in 32 GB: %d", got)
	}
	if _, err := sandbox.ParseMemory("lots"); err == nil {
		t.Error("lots isn't a size")
	}
}
