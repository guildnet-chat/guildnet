// SPDX-License-Identifier: AGPL-3.0-only

// Package limit slows down guessing and floods: after so many tries with a
// key (an address, an account) within a window, the next are refused until
// the window passes.
package limit

import (
	"sync"
	"time"
)

// Tries counts tries by key. Its methods are safe to call at once.
type Tries struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	tries  map[string][]time.Time
	swept  time.Time // when keys whose tries passed were last swept out
}

// New counts tries: after max within window, a key is refused.
func New(max int, window time.Duration) *Tries {
	return &Tries{max: max, window: window, tries: map[string][]time.Time{}}
}

// keys is how many keys Tries holds before it sweeps out the ones whose
// tries have all passed: at most once every sweepEvery, so made-up keys (a
// sign-in with a new name each time) can't have every try walk them all.
const (
	keys       = 10_000
	sweepEvery = time.Second
)

func (l *Tries) recent(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.tries[key] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(l.tries, key)
	} else {
		l.tries[key] = keep
	}
	return keep
}

// Blocked says whether key has had its tries for now.
func (l *Tries) Blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, now)) >= l.max
}

// Try counts a try with key and says whether it may go ahead: false once
// key has had its tries, and then it isn't counted. It's counted before
// whatever it guards is checked, so tries made at once can't all slip in
// before any is counted. Reset forgets them once one succeeds.
func (l *Tries) Try(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent(key, now)) >= l.max {
		return false
	}
	if len(l.tries) >= keys && (now.Sub(l.swept) >= sweepEvery || now.Before(l.swept)) {
		l.swept = now
		for k := range l.tries {
			l.recent(k, now)
		}
	}
	l.tries[key] = append(l.recent(key, now), now)
	return true
}

// Minute counts calls by key, a minute (of the clock) at a time. Its zero
// value is ready to use, and its methods are safe to call at once.
type Minute struct {
	mu     sync.Mutex
	minute int64
	calls  map[string]int
}

// Call counts a call with key, or says to wait (and for how long) if it's
// one more than max this minute.
func (l *Minute) Call(key string, now time.Time, max int) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m := now.Unix() / 60; m != l.minute || l.calls == nil {
		l.minute, l.calls = m, map[string]int{}
	}
	if l.calls[key] >= max {
		return false, time.Duration(60-now.Unix()%60) * time.Second
	}
	l.calls[key]++
	return true, 0
}

// Reset forgets key's tries.
func (l *Tries) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tries, key)
}

// Len is how many keys it holds tries of.
func (l *Tries) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.tries)
}

// Locks are locks by key, kept only while someone holds or waits for one
// (so made-up keys leave nothing behind). Its zero value is ready to use.
type Locks struct {
	mu   sync.Mutex
	held map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	users int // holding it, or waiting to
}

// Lock holds key's lock until the func it returns is called.
func (k *Locks) Lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.held == nil {
		k.held = map[string]*keyLock{}
	}
	l := k.held[key]
	if l == nil {
		l = &keyLock{}
		k.held[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.held, key)
		}
		k.mu.Unlock()
	}
}

// Len is how many keys it holds locks for.
func (k *Locks) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.held)
}
