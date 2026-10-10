// SPDX-License-Identifier: AGPL-3.0-only

package limit_test

import (
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/limit"
)

func TestCallsAreCountedAMinuteAtATime(t *testing.T) {
	var l limit.Minute
	now := time.Date(2026, 10, 2, 12, 0, 15, 0, time.UTC)
	for i := range 3 {
		if ok, _ := l.Call("ada", now, 3); !ok {
			t.Fatalf("call %d of 3 refused", i+1)
		}
	}
	ok, retry := l.Call("ada", now, 3)
	if ok || retry != 45*time.Second {
		t.Errorf("a fourth call: %v, retry in %v, want refused until the minute's out (45s)", ok, retry)
	}
	if ok, _ := l.Call("bo", now, 3); !ok {
		t.Error("another key's call refused")
	}
	if ok, _ := l.Call("ada", now.Add(time.Minute), 3); !ok {
		t.Error("refused in the next minute")
	}
}
