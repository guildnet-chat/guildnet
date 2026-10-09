// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// GuildNet is under the AGPL: whoever runs a changed copy for others offers
// them its source (section 13). The console links to it, where it's
// published (Source), from every page: the home page's site, a network's
// console, and its sign-in; and nowhere when it isn't said.
func TestEveryPageLinksToTheSource(t *testing.T) {
	const source = "https://git.example.com/acme/guildnet"
	link := `href="` + source + `"`
	pages := func(w *World) map[string]Response {
		return map[string]Response{
			"the home page's sign-in": w.Home().Get("/signin"),
			"your networks":           SignedInHome(w, "bob@example.com").Get("/networks"),
			"the console":             w.Browser("acme").SignedIn("ada").Get("/"),
			"the console's sign-in":   w.Browser("acme").Get("/signin"),
		}
	}

	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.Source = source })
	for name, page := range pages(w) {
		if !strings.Contains(page.Body, link) {
			t.Errorf("%s has no link to the source:\n%s", name, page.Text())
		}
	}

	w = NewWorld(t)
	for name, page := range pages(w) {
		if strings.Contains(page.Text(), "Source") {
			t.Errorf("%s says Source, with nowhere said:\n%s", name, page.Text())
		}
	}
}
