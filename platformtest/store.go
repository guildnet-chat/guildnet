// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/guildnet-chat/guildnet/pgtest"
	"github.com/guildnet-chat/guildnet/platform"
)

// pg is the throwaway Postgres that worlds and stores keep things in; nil
// when Docker isn't running.
var pg *pgtest.Server

// Main runs a package's tests (call it from TestMain), with a throwaway
// Postgres for them when Docker is running.
func Main(m *testing.M) {
	var err error
	pg, err = pgtest.Start()
	if err != nil && !errors.Is(err, pgtest.ErrNoDocker) {
		panic(err)
	}
	code := m.Run()
	if pg != nil {
		pg.Stop()
	}
	os.Exit(code)
}

// TestKey seals secrets in tests: 32 bytes, as in production.
var TestKey = bytes.Repeat([]byte{7}, 32)

// NewDatabase is a new, empty database's address; the test skips without
// Docker.
func NewDatabase(t *testing.T) string {
	t.Helper()
	if pg == nil {
		t.Skip("Docker isn't running; skipping Postgres")
	}
	url, err := pg.NewDatabase()
	if err != nil {
		t.Fatal(err)
	}
	return url
}

// MustSealer seals with key, or fails the test.
func MustSealer(t *testing.T, key []byte) platform.Sealer {
	t.Helper()
	s, err := platform.NewAESSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
