// SPDX-License-Identifier: AGPL-3.0-only

package pgtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/pgtest"
)

// fakeT records what a check finds, without failing the test running it.
type fakeT struct {
	testing.TB
	found []string
}

func (f *fakeT) Helper()                        {}
func (f *fakeT) Errorf(format string, a ...any) { f.found = append(f.found, format) }
func (f *fakeT) Fatalf(format string, a ...any) { f.found = append(f.found, format) }

func migrations(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, sql := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func check(t *testing.T, dir string, after int, version string) []string {
	f := &fakeT{TB: t}
	pgtest.ExpandOnly(f, dir, after, version)
	return f.found
}

// While a deploy hands over, the old copy runs on the new schema for a
// while: a migration doesn't drop or rename what the release before it uses.
func TestAMigrationThatDropsWhatsStillUsedIsRefused(t *testing.T) {
	dir := migrations(t, map[string]string{
		"00031_tidy.sql": "-- +goose Up\nALTER TABLE sessions DROP COLUMN idle_until;\n-- +goose Down\nSELECT 1;\n",
	})
	if found := check(t, dir, 30, "26.10.2"); len(found) != 1 {
		t.Errorf("found %v, want the drop refused", found)
	}
}

func TestARenameIsRefusedToo(t *testing.T) {
	dir := migrations(t, map[string]string{
		"00031_rename.sql": "-- +goose Up\nalter table links rename column lane to team;\n",
	})
	if found := check(t, dir, 30, "26.10.2"); len(found) != 1 {
		t.Errorf("found %v, want the rename refused", found)
	}
}

// One after the release that stopped using it (out already) may.
func TestADropAfterTheReleaseThatStoppedUsingItIsAllowed(t *testing.T) {
	dir := migrations(t, map[string]string{
		"00031_tidy.sql": "-- +goose Up\n-- After release 26.10.1, which stopped using it.\nALTER TABLE sessions DROP COLUMN idle_until;\n",
	})
	if found := check(t, dir, 30, "26.10.2"); len(found) != 0 {
		t.Errorf("found %v, want it allowed", found)
	}
}

// Naming a release not out yet doesn't do.
func TestADropAfterAReleaseNotOutYetIsRefused(t *testing.T) {
	dir := migrations(t, map[string]string{
		"00031_tidy.sql": "-- +goose Up\n-- After release 26.11.0, which stopped using it.\nDROP TABLE old_things;\n",
	})
	if found := check(t, dir, 30, "26.10.2"); len(found) != 1 {
		t.Errorf("found %v, want it refused", found)
	}
}

// Undoing (Down) may drop what Up made; migrations from before the rule
// are as they were; adding is always fine.
func TestDownsOldOnesAndAddingAreFine(t *testing.T) {
	dir := migrations(t, map[string]string{
		"00030_old.sql": "-- +goose Up\nALTER TABLE x DROP COLUMN y;\n",
		"00031_add.sql": "-- +goose Up\nALTER TABLE x ADD COLUMN z text;\nCREATE TABLE w (id int);\n-- +goose Down\nDROP TABLE w;\nALTER TABLE x DROP COLUMN z;\n",
	})
	if found := check(t, dir, 30, "26.10.2"); len(found) != 0 {
		t.Errorf("found %v, want nothing", found)
	}
}

// The checks GuildNet's own migrations are held to.
func TestGuildNetsMigrationsAddBeforeTheyTakeAway(t *testing.T) {
	version, err := os.ReadFile(filepath.Join("..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	v := strings.TrimSpace(string(version))
	pgtest.ExpandOnly(t, filepath.Join("..", "platform", "migrations"), 30, v)
	pgtest.ExpandOnly(t, filepath.Join("..", "internal", "org", "migrations"), 17, v)
}
