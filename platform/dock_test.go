// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Chat stays open while you use the console: it's docked beside every page,
// and moving between pages swaps only the page (and the sidebar), never the
// chat, so its connection carries on.

func TestChatIsDockedBesideEveryConsolePage(t *testing.T) {
	w := newWorldWithChatApp(t)
	ada := w.Browser("acme").SignedIn("ada")
	for _, path := range []string{"/", "/teams", "/approvals", "/connect"} {
		r := ada.Get(path)
		if strings.Count(r.Body, `src="/chat/app/"`) != 1 || !strings.Contains(r.Body, `id="chat-dock"`) {
			t.Errorf("%s: no chat docked beside it", path)
		}
		if !strings.Contains(r.Header.Get("Content-Security-Policy"), "frame-src 'self'") {
			t.Errorf("%s: the page may not frame the chat: %s", path, r.Header.Get("Content-Security-Policy"))
		}
		if !strings.Contains(r.Body, `id="page"`) || !strings.Contains(r.Body, `id="nav"`) {
			t.Errorf("%s: the page and the sidebar aren't marked out for swapping", path)
		}
	}
}

func TestMovingBetweenPagesSwapsOnlyThePageAndTheSidebar(t *testing.T) {
	w := newWorldWithChatApp(t)
	body := w.Browser("acme").SignedIn("ada").Get("/").Body
	tag := regexp.MustCompile(`<body[^>]*>`).FindString(body)
	for _, want := range []string{`hx-boost:inherited="true"`, `hx-target:inherited="#page"`, `hx-select:inherited="#page"`,
		`hx-swap:inherited="outerHTML"`, `hx-select-oob:inherited="#nav"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("the console's %s has no %s", tag, want)
		}
	}
	// Forms that leave the console's frame are ordinary forms.
	if !regexp.MustCompile(`<form[^>]*action="/signout"[^>]*hx-boost="false"`).MatchString(body) {
		t.Error("signing out is swapped into the page")
	}
}

func TestTheChatPageWidensTheDock(t *testing.T) {
	w := newWorldWithChatApp(t)
	r := w.Browser("acme").SignedIn("ada").Get("/chat/")
	if r.Status != http.StatusOK || !regexp.MustCompile(`id="page"[^>]*data-chat="wide"`).MatchString(r.Body) {
		t.Errorf("the chat page doesn't widen the dock: %d", r.Status)
	}
	if strings.Count(r.Body, `src="/chat/app/"`) != 1 {
		t.Errorf("the chat page has %d chat apps, want one", strings.Count(r.Body, `src="/chat/app/"`))
	}
}

// Someone who must set up two-step sign-in first can't use web chat yet, so
// no chat is docked beside their pages (the setting up page above all, whose
// recovery codes show only once); once it's set up, it is. Each page says
// whether it has the chat docked beside it, so moving to one that differs
// (pages swap without reloading the dock) loads the console afresh.
func TestChatIsDockedOnceTwoStepIsSetUp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!DOCTYPE html><title>gamja</title><p>app"), 0o644)
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, RequireTwoStep, func(o *Setup) { o.ChatDir = dir })
	ada := w.Browser("acme").SignedIn("ada")
	docked := regexp.MustCompile(`id="page"[^>]*data-chat-dock="on"`)

	for _, path := range []string{"/account/two-step", "/account"} {
		r := ada.Get(path)
		if strings.Contains(r.Body, `id="chat-dock"`) || docked.MatchString(r.Body) {
			t.Errorf("%s before two-step is set up: the chat is docked", path)
		}
	}
	ada.SetUpTwoStep()
	r := ada.Get("/")
	if !strings.Contains(r.Body, `id="chat-dock"`) || !docked.MatchString(r.Body) {
		t.Errorf("once two-step is set up: no chat docked (or the page doesn't say so)")
	}
	if r := ada.Get("/account/two-step"); strings.Contains(r.Body, `id="chat-dock"`) || docked.MatchString(r.Body) {
		t.Error("the two-step page, which may show recovery codes once, has the chat docked")
	}

}

func TestWithoutWebChatThereIsNoDock(t *testing.T) {
	w := NewWorld(t) // no chat app here
	r := w.Browser("acme").SignedIn("ada").Get("/")
	if strings.Contains(r.Body, `id="chat-dock"`) || strings.Contains(r.Header.Get("Content-Security-Policy"), "frame-src") {
		t.Error("a chat dock, though web chat isn't set up")
	}
}
