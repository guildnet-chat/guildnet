// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A desk's home lasts between wake-ups, so what's in it can't stand in for
// the system's commands: the PATH looks in the image's own folders first,
// and in the home's (go install's, pip --user's) last.
func TestADesksHomeComesLastOnItsPath(t *testing.T) {
	b, err := os.ReadFile("../desk/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)\bPATH=(\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the desk's Dockerfile sets no PATH")
	}
	dirs := strings.Split(string(m[1]), ":")
	seenHome := false
	for _, d := range dirs {
		home := strings.HasPrefix(d, "/home/") || strings.HasPrefix(d, "$HOME") || strings.HasPrefix(d, "$GOPATH")
		if seenHome && !home {
			t.Errorf("the PATH %s looks in the home before %s", m[1], d)
		}
		seenHome = seenHome || home
	}
}
