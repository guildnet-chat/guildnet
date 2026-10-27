// SPDX-License-Identifier: AGPL-3.0-only

package limit_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/limit"
)

// Made-up keys (a sign-in with a new name each time) can't slow everyone's
// tries: with tens of thousands held, a try is still quick.
func TestManyKeysDontSlowATry(t *testing.T) {
	l := limit.New(5, 15*time.Minute)
	now := time.Now()
	for i := range 50_000 {
		l.Try(fmt.Sprintf("acme|made-up-%d", i), now)
	}

	start := time.Now()
	for i := range 2_000 {
		l.Try(fmt.Sprintf("acme|another-%d", i), now)
	}

	if took := time.Since(start); took > time.Second {
		t.Errorf("2,000 tries with 50,000 keys held took %v", took)
	}
	if !l.Try("acme|ada", now) || l.Blocked("acme|ada", now) {
		t.Error("a real key is held back")
	}
}

// Keys whose tries have all passed still go, in time.
func TestKeysWhoseTriesPassedGo(t *testing.T) {
	l := limit.New(5, time.Minute)
	now := time.Now()
	for i := range 20_000 {
		l.Try(fmt.Sprintf("k%d", i), now)
	}

	later := now.Add(2 * time.Minute)
	l.Try("new", later)

	if n := l.Len(); n > 1 {
		t.Errorf("%d keys held after their tries passed, want just the new one", n)
	}
}

// Locks: one holder of a key at a time, and a key nobody holds isn't kept.
func TestALockIsOneAtATimeAndForgottenOnceFree(t *testing.T) {
	var locks limit.Locks
	var in, most atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := locks.Lock("acme|ada")
			if n := in.Add(1); n > most.Load() {
				most.Store(n)
			}
			time.Sleep(time.Millisecond)
			in.Add(-1)
			unlock()
		}()
	}
	wg.Wait()

	if most.Load() != 1 {
		t.Errorf("%d held the lock at once", most.Load())
	}
	for i := range 1_000 {
		locks.Lock(fmt.Sprintf("acme|made-up-%d", i))()
	}
	if n := locks.Len(); n != 0 {
		t.Errorf("%d locks kept that nobody holds", n)
	}
}
