// SPDX-License-Identifier: AGPL-3.0-only

package limit_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/limit"
)

func TestTriesAreRefusedUntilTheWindowPasses(t *testing.T) {
	l := limit.New(2, time.Hour)
	now := time.Now()
	if !l.Try("ada", now) || l.Blocked("ada", now) {
		t.Fatal("one try of two is blocked")
	}
	if !l.Try("ada", now) || !l.Blocked("ada", now) {
		t.Fatal("two tries of two aren't blocked")
	}
	if l.Try("ada", now) {
		t.Error("a third try of two was allowed")
	}
	if l.Blocked("bo", now) {
		t.Error("another key is blocked")
	}
	if !l.Try("ada", now.Add(time.Hour)) {
		t.Error("still blocked once the window passed")
	}
	l.Reset("ada")
	if l.Blocked("ada", now) {
		t.Error("still blocked after a reset")
	}
}

// A try is counted as it's allowed, so many at once can't all slip in
// before any is counted.
func TestTriesAtOnceGetNoMoreThanTheirShare(t *testing.T) {
	l := limit.New(5, time.Hour)
	now := time.Now()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Try("ada", now) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 5 {
		t.Errorf("%d of 200 tries at once were allowed, want 5", allowed.Load())
	}
	if !l.Blocked("ada", now) {
		t.Error("ada isn't blocked after her five")
	}
	l.Reset("ada")
	if !l.Try("ada", now) {
		t.Error("refused after a reset")
	}
}
