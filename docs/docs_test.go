// SPDX-License-Identifier: AGPL-3.0-only

// Package docs is GuildNet's documentation; its tests keep it in step with
// the code: every setting is described, and every link leads somewhere.
package docs_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// settings is every GUILDNET_ setting the open code reads, and every one the
// self-host stack (compose.yml, init.sh) uses.
func settings(t *testing.T) []string {
	t.Helper()
	read := regexp.MustCompile(`"(GUILDNET_[A-Z0-9_]+)"`)
	used := regexp.MustCompile(`\$\{?(GUILDNET_[A-Z0-9_]+)|^\s*-?\s*(GUILDNET_[A-Z0-9_]+)[:=]`)
	var names []string
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Test helpers and fakes aren't GuildNet's settings.
			if slices.Contains([]string{"platformtest", "ergotest", "ircfake", "pgtest", "node_modules", ".git"}, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"):
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range read.FindAllStringSubmatch(string(b), -1) {
				add(m[1])
			}
		case path == filepath.Join("..", "deploy", "selfhost", "compose.yml") || path == filepath.Join("..", "deploy", "selfhost", "init.sh") ||
			path == filepath.Join("..", "deploy", "ergo", "start.sh"):
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(b), "\n") {
				for _, m := range used.FindAllStringSubmatch(line, -1) {
					add(m[1])
					add(m[2])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 50 {
		t.Fatalf("found only %d settings: is the walk right?", len(names))
	}
	return names
}

// Every setting is in the configuration reference.
func TestEverySettingIsDescribed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("self-host", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	ref := string(b)
	for _, name := range settings(t) {
		if !strings.Contains(ref, "`"+name+"`") {
			t.Errorf("configuration.md doesn't describe %s", name)
		}
	}
}

// slug is the anchor GitHub gives a heading.
func slug(heading string) string {
	s := strings.ToLower(strings.TrimSpace(heading))
	s = regexp.MustCompile(`[^a-z0-9 _-]`).ReplaceAllString(s, "")
	return strings.ReplaceAll(s, " ", "-")
}

// A table ends with a blank line: text straight after it is read as one
// more row.
func TestEveryTableEndsWithABlankLine(t *testing.T) {
	pages, _ := filepath.Glob(filepath.Join("self-host", "*.md"))
	more, _ := filepath.Glob(filepath.Join("..", "*.md"))
	docs, _ := filepath.Glob("*.md")
	pages = append(append(pages, more...), docs...)
	for _, page := range pages {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		for i := 1; i < len(lines); i++ {
			if strings.HasPrefix(lines[i-1], "|") && lines[i] != "" && !strings.HasPrefix(lines[i], "|") {
				t.Errorf("%s:%d: text straight after a table", page, i+1)
			}
		}
	}
}

// Every link between the self-hosting docs (and from the READMEs and the
// community files to them) leads to a file that's there, and to a heading
// in it if it names one.
func TestEveryLinkLeadsSomewhere(t *testing.T) {
	pages, _ := filepath.Glob(filepath.Join("self-host", "*.md"))
	docs, _ := filepath.Glob("*.md")
	pages = append(pages, docs...)
	pages = append(pages, filepath.Join("..", "README.md"), filepath.Join("..", "deploy", "selfhost", "README.md"))
	for _, f := range []string{"CONTRIBUTING.md", "SECURITY.md", "TRADEMARKS.md", "CODE_OF_CONDUCT.md", filepath.Join(".github", "PULL_REQUEST_TEMPLATE.md")} {
		pages = append(pages, filepath.Join("..", f))
	}
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	heading := regexp.MustCompile(`(?m)^#+ (.+)$`)
	for _, page := range pages {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range link.FindAllStringSubmatch(string(b), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			path := page
			if file != "" {
				path = filepath.Join(filepath.Dir(page), filepath.FromSlash(file))
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Errorf("%s links to %s, which isn't there", page, target)
				continue
			}
			if anchor == "" || info.IsDir() {
				continue
			}
			to, _ := os.ReadFile(path)
			var anchors []string
			for _, h := range heading.FindAllStringSubmatch(string(to), -1) {
				anchors = append(anchors, slug(h[1]))
			}
			if !slices.Contains(anchors, anchor) {
				t.Errorf("%s links to %s, which has no heading %q", page, target, anchor)
			}
		}
	}
}
