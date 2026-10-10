// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A console or chat left open since before a deploy is an older GuildNet:
// each page says which version it is, so the console reloads itself on the
// next click after one, and chat offers to reload.

var pageVersion = regexp.MustCompile(`id="page"[^>]*data-version="([0-9a-f]{12,})"`)
var chatVersion = regexp.MustCompile(`<meta name="guildnet-version" content="([0-9a-f]{12,})">`)

func chatWorld(t *testing.T, index string) *World {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644)
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.ChatDir = dir })
}

func TestConsolePagesSayWhichVersionTheyAre(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	org := pageVersion.FindStringSubmatch(ada.Get("/").Body)
	team := pageVersion.FindStringSubmatch(ada.Get("/teams/Sales").Body)

	if org == nil || team == nil || org[1] != team[1] {
		t.Errorf("every console page should carry the same version: %v, %v", org, team)
	}
}

func TestChatSaysWhichVersionItIsAndWhichIsCurrent(t *testing.T) {
	w := chatWorld(t, `<!doctype html><head><title>gamja</title></head><body></body>`)
	ada := w.Browser("acme").SignedIn("ada")

	loaded := chatVersion.FindStringSubmatch(ada.Get("/chat/app/").Body)
	now := ada.Get("/chat/app/version")
	var current struct {
		Version string `json:"version"`
	}
	json.Unmarshal([]byte(now.Body), &current)

	if loaded == nil || current.Version != loaded[1] {
		t.Errorf("the chat page's version %v should be the current one, %q", loaded, now.Body)
	}
	if now.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q; the current version mustn't be cached", now.Header.Get("Cache-Control"))
	}
	if console := pageVersion.FindStringSubmatch(ada.Get("/").Body); console == nil || console[1] != current.Version {
		t.Error("the console and chat are one deploy, so one version")
	}
}

// A new build of chat is a new version, though nothing else changed.
func TestANewChatBuildIsANewVersion(t *testing.T) {
	before := chatWorld(t, `<!doctype html><head><script src="app.1498919c.js"></script></head>`)
	after := chatWorld(t, `<!doctype html><head><script src="app.2104e0e9.js"></script></head>`)

	v1 := chatVersion.FindStringSubmatch(before.Browser("acme").SignedIn("ada").Get("/chat/app/").Body)
	v2 := chatVersion.FindStringSubmatch(after.Browser("acme").SignedIn("ada").Get("/chat/app/").Body)

	if v1 == nil || v2 == nil || v1[1] == v2[1] {
		t.Errorf("versions %v and %v should differ", v1, v2)
	}
	if strings.Contains(v1[0], "1498919c") {
		t.Error("the version is a hash, not a file name")
	}
}
