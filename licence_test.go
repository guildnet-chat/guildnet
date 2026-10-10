// SPDX-License-Identifier: AGPL-3.0-only

package guildnet_test

import (
	"bufio"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
)

// The licences: GuildNet's under the AGPL, what you'd copy into your own
// code (examples/) under Apache-2.0.
func TestTheLicencesAreThere(t *testing.T) {
	for file, says := range map[string][]string{
		"LICENSE":          {"GNU AFFERO GENERAL PUBLIC LICENSE", "Version 3, 19 November 2007"},
		"examples/LICENSE": {"Apache License", "Version 2.0, January 2004"},
	} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		for _, s := range says {
			if !strings.Contains(string(body), s) {
				t.Errorf("%s doesn't say %q", file, s)
			}
		}
	}
}

// Code that isn't ours, shipped as it came: its notices are in NOTICE.
var theirs = map[string]string{
	"platform/static/chart-4.5.1.umd.min.js": "Chart.js",
	"platform/static/htmx-4.0.0.min.js":      "htmx",
}

func TestEverySourceFileSaysItsLicence(t *testing.T) {
	files := tracked(t)
	n := 0
	for _, f := range files {
		want, ok := licenceOf(f)
		if !ok {
			continue
		}
		n++
		if got := spdx(t, f); got != want {
			t.Errorf("%s: SPDX-License-Identifier %q, want %q", f, got, want)
		}
	}
	if n < 300 {
		t.Fatalf("only %d source files: is the list right?", n)
	}
}

func TestNoticeNamesWhatIsntOurs(t *testing.T) {
	body, err := os.ReadFile("NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	for f, project := range theirs {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s: %v (update theirs)", f, err)
		}
		if !strings.Contains(string(body), project) {
			t.Errorf("NOTICE doesn't name %s, for %s", project, f)
		}
	}
	for _, project := range []string{"Ergo", "JetBrains Mono", "Space Grotesk"} {
		if !strings.Contains(string(body), project) {
			t.Errorf("NOTICE doesn't name %s", project)
		}
	}
}

// licenceOf is the licence a file of ours should say it's under, and
// whether it's a file that says one: code, scripts, styles, templates and
// the stack's own settings. Sample networks (guildnet.yaml, personas/,
// templates/) are data people make their own, and say nothing.
func licenceOf(f string) (string, bool) {
	if _, ok := theirs[f]; ok {
		return "", false
	}
	base := path.Base(f)
	switch {
	case f == "guildnet.yaml" || strings.HasPrefix(f, "personas/") || strings.HasPrefix(f, "templates/"):
		return "", false
	case f == "deploy/ergo/ircd.yaml":
		// Ergo's own default.yaml (MIT), changed for GuildNet.
		return "AGPL-3.0-only AND MIT", true
	}
	switch path.Ext(f) {
	case ".go", ".sh", ".sql", ".js", ".css", ".html", ".py", ".yml", ".yaml":
	default:
		if base != "Dockerfile" && base != "Caddyfile" {
			return "", false
		}
	}
	if strings.HasPrefix(f, "examples/") {
		return "Apache-2.0", true
	}
	return "AGPL-3.0-only", true
}

// spdx is what the file's SPDX-License-Identifier says, in its first five
// lines.
func spdx(t *testing.T, f string) string {
	t.Helper()
	file, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	s := bufio.NewScanner(file)
	for i := 0; i < 5 && s.Scan(); i++ {
		if _, after, ok := strings.Cut(s.Text(), "SPDX-License-Identifier: "); ok {
			id := strings.TrimSpace(after)
			for _, end := range []string{"*/ -}}", "*/"} { // a template's comment, or a style sheet's
				id = strings.TrimSpace(strings.TrimSuffix(id, end))
			}
			return id
		}
	}
	return ""
}

// tracked are the files git has here, by their paths from here.
func tracked(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout (%v): nothing to check", err)
	}
	return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
}
