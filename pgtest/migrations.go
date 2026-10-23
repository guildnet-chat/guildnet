// SPDX-License-Identifier: AGPL-3.0-only

package pgtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// While a deploy hands over (guildnet.chat's console; docs/development.md),
// the old copy runs for a while on the schema the new copy migrated to. So
// a migration adds, and takes away only what an earlier release stopped
// using: a drop or rename says so, naming that release, which must be out
// already.

var (
	takesAway = regexp.MustCompile(`(?i)\b(drop\s+(table|column)|rename\s+(column|to))\b`)
	after     = regexp.MustCompile(`(?im)^--\s*After release (\d+\.\d+\.\d+), which stopped using it\.`)
	numbered  = regexp.MustCompile(`^(\d+)_.*\.sql$`)
)

// ExpandOnly checks the goose migrations in dir numbered above from (the
// ones before the rule are as they were) against it: one whose Up drops or
// renames must name the release, version or before, that stopped using
// what it takes away.
func ExpandOnly(t testing.TB, dir string, from int, version string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
		return
	}
	for _, e := range entries {
		m := numbered.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if n, _ := strconv.Atoi(m[1]); n <= from {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
			return
		}
		up := string(b)
		if i := strings.Index(up, "-- +goose Down"); i >= 0 {
			up = up[:i]
		}
		if !takesAway.MatchString(up) {
			continue
		}
		said := after.FindStringSubmatch(up)
		switch {
		case said == nil:
			t.Errorf("%s drops or renames: say after which release, out already, that stopped using it (-- After release X.Y.Z, which stopped using it.)", e.Name())
		case !releasedBy(said[1], version):
			t.Errorf("%s takes away what release %s stopped using, which isn't out yet (the latest is %s)", e.Name(), said[1], version)
		}
	}
}

// releasedBy says whether release a is version or before it.
func releasedBy(a, version string) bool {
	x, y := strings.Split(a, "."), strings.Split(version, ".")
	for i := range x {
		if i >= len(y) {
			return false
		}
		p, _ := strconv.Atoi(x[i])
		q, _ := strconv.Atoi(y[i])
		if p != q {
			return p < q
		}
	}
	return true
}
