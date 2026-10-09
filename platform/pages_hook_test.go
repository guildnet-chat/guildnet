// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// An extension brings its own pages and static files: pages of the public
// site, written whole, and pages in the home page's frame, each with the
// console's shared parts (the site's header and footer, its icons) and
// functions of the extension's own. Its static files are served beside the
// console's, at addresses that change when they do.

var tourFiles = fstest.MapFS{
	"tour.html": {Data: []byte(`<!doctype html><title>{{brand}} tour</title>
<link rel="stylesheet" href="{{static "tour.css"}}">
{{template "site-header" .Site}}<p>{{.Data}}</p><p>{{shout "hi"}}</p>{{template "aside"}}`)},
	"house.html": {Data: []byte(`{{define "title"}}The house · {{brand}}{{end}}
{{define "main"}}<h1>The house</h1><p>{{.Data}}</p>{{template "aside"}}{{end}}`)},
	"aside.html": {Data: []byte(`{{define "aside"}}<aside>an aside</aside>{{end}}`)},
}

var tourStatic = fstest.MapFS{"tour.css": {Data: []byte("body { color: teal }")}}

func withTour(o *Setup) {
	o.Extension = platform.ExtensionFunc(func(c *platform.Core) platform.Hooks {
		return platform.Hooks{
			Pages: &platform.Pages{
				FS: tourFiles, Site: []string{"tour"}, Home: []string{"house"}, Partials: []string{"aside"}, Static: tourStatic,
				Funcs: template.FuncMap{"shout": func(s string) string { return strings.ToUpper(s) + "!" }},
			},
			Home: func(routes platform.Routes) {
				routes.Handle("GET /tour", platform.Anyone, func(w http.ResponseWriter, r *http.Request) {
					c.Render(w, r, http.StatusOK, "tour", "the grand tour")
				})
				routes.Handle("GET /house", platform.SignedIn, func(w http.ResponseWriter, r *http.Request) {
					c.RenderHome(w, r, http.StatusOK, "house", "yours to keep")
				})
			},
		}
	})
}

func TestAnExtensionBringsItsOwnPages(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, withTour)

	tour := w.Home().Get("/tour")
	for _, want := range []string{"<title>GuildNet tour</title>", `class="site-header"`, "the grand tour", "HI!", "<aside>an aside</aside>"} {
		if !strings.Contains(tour.Body, want) {
			t.Errorf("the tour doesn't have %s:\n%s", want, tour.Body)
		}
	}
	css := regexp.MustCompile(`href="(/static/tour\.css\?v=[0-9a-f]+)"`).FindStringSubmatch(tour.Body)
	if css == nil {
		t.Fatalf("the tour doesn't link its stylesheet at a versioned address:\n%s", tour.Body)
	}
	if r := w.Home().Get(css[1]); r.Status != http.StatusOK || r.Body != "body { color: teal }" || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("its stylesheet: %d %q %q", r.Status, r.Body, r.Header.Get("Cache-Control"))
	}
	// The console's own static files are still there.
	if r := w.Home().Get("/favicon.ico"); r.Status != http.StatusOK {
		t.Errorf("the console's icon: %d", r.Status)
	}

	house := w.OnHome("ada@example.com").Get("/house")
	for _, want := range []string{"<title>The house · GuildNet</title>", "<h1>The house</h1>", "yours to keep", "<aside>an aside</aside>", `action="/signout"`} {
		if !strings.Contains(house.Body, want) {
			t.Errorf("the house doesn't have %s:\n%s", want, house.Body)
		}
	}
}

// An extension can put its own header and footer on the site, the
// console's pages included.
func TestAnExtensionFramesTheSite(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Extension = platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
			return platform.Hooks{Pages: &platform.Pages{FS: fstest.MapFS{"frame.html": {Data: []byte(
				`{{define "site-header"}}<header>the house's header{{if .SignIn}}, sign in{{end}}</header>{{end}}{{define "site-footer"}}<footer>the house's footer</footer>{{end}}`)}},
				Frame: "frame"}}
		})
	})

	page := w.Home().Get("/signin")
	for _, want := range []string{"<header>the house's header, sign in</header>", "<footer>the house's footer</footer>"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("sign-in doesn't have %s:\n%s", want, page.Body)
		}
	}
	if strings.Contains(page.Body, `class="site-header"`) {
		t.Errorf("sign-in still has the console's own header:\n%s", page.Body)
	}
}

// A page or a static file the console has already is a mistake: the
// platform won't start.
func TestAnExtensionCantTakeTheConsolesNames(t *testing.T) {
	for name, pages := range map[string]*platform.Pages{
		"a page":        {FS: fstest.MapFS{"signin.html": {Data: []byte(`{{define "main"}}{{end}}`)}}, Home: []string{"signin"}},
		"a static file": {FS: fstest.MapFS{}, Static: fstest.MapFS{"favicon.ico": {Data: []byte("x")}}},
		"a function":    {FS: fstest.MapFS{}, Funcs: template.FuncMap{"brand": func() string { return "ours" }}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("an extension took the console's %s", name)
				}
			}()
			platform.New(platform.Options{BaseDomain: BaseDomain, Networks: platform.NewMemoryStore(), Sessions: platform.NewMemoryStore(),
				Extension: platform.ExtensionFunc(func(*platform.Core) platform.Hooks { return platform.Hooks{Pages: pages} })})
		})
	}
}
