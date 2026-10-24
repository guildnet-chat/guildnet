// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/x509"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Beside the catalogue's, a network's owner adds MCP servers of their own,
// from the console: one with the MCP sign-in is connected that way, any
// other with a token the owner pastes (an API key), tried on the server
// first. Every tool of one asks a person first (the runner's side), and it
// must be on the internet unless the install names its host.

// wikiServer is a company's own MCP server, which takes one token.
func wikiServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "wiki", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "search_pages", Description: "Search the wiki"}, func(context.Context, *mcp.CallToolRequest, struct {
		Query string `json:"query"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Onboarding"}}}, nil, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer wiki-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// customWorld has the fake vendor (with its sign-in) and the wiki (without),
// both trusted as the platform reaches services.
func customWorld(t *testing.T) (*World, *fakeVendor, *httptest.Server) {
	t.Helper()
	f := newFakeVendor(t)
	wiki := wikiServer(t)
	s := platform.NewMemoryStore()
	w := NewWorldWith(t, func() Store { return s }, withVendor(f), func(o *Setup) {
		pool := x509.NewCertPool()
		pool.AddCert(f.srv.Certificate())
		pool.AddCert(wiki.Certificate())
		// The test servers are on this machine: named, as a self-hoster names a
		// server on their own network.
		o.ConnectorEgress = egress.Options{RootCAs: pool, AllowLoopback: true, PrivateHosts: []string{"127.0.0.1"}}
	})
	return w, f, wiki
}

func addCustom(b *Browser, title, address, token string) Response {
	b.Get("/connectors")
	return b.Post("/connectors/custom", url.Values{"title": {title}, "url": {address}, "token": {token}})
}

func TestTheOwnerAddsAnMCPServerOfTheirOwnWithAToken(t *testing.T) {
	w, _, wiki := customWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	if page := ada.Get("/connectors"); !strings.Contains(page.Body, `action="/connectors/custom"`) {
		t.Fatalf("the owner should be offered adding a server of their own:\n%s", VisibleText(page))
	}

	r := addCustom(ada, "Acme Wiki", wiki.URL+"/mcp", "wiki-token")

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/connectors#acme-wiki" {
		t.Fatalf("adding the wiki: %d to %q\n%s", r.Status, r.Header.Get("Location"), VisibleText(r))
	}
	c, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "acme-wiki")
	if err != nil || c.Custom == nil || c.Custom.URL != wiki.URL+"/mcp" || c.Custom.Title != "Acme Wiki" || c.Tokens.Access != "wiki-token" {
		t.Fatalf("acme's wiki: %+v, %v", c, err)
	}
	_, list := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "GET", "/connectors")
	found := false
	for _, x := range list["connectors"].([]any) {
		e := x.(map[string]any)
		if e["id"] == "acme-wiki" {
			found = e["custom"] == true && e["url"] == wiki.URL+"/mcp" && e["title"] == "Acme Wiki"
		}
	}
	if !found {
		t.Errorf("the runner's list should have the wiki, as custom: %v", list)
	}
	if status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/connectors/acme-wiki/token"); status != http.StatusOK || out["token"] != "wiki-token" {
		t.Errorf("the runner's token for the wiki: %d %v", status, out)
	}
	page := ada.Get("/connectors")
	if !strings.Contains(VisibleText(page), "Acme Wiki") || !strings.Contains(page.Body, `action="/connectors/acme-wiki/disconnect"`) {
		t.Errorf("the page should list the wiki, with a way to disconnect it:\n%s", VisibleText(page))
	}

	if r := ada.Post("/connectors/acme-wiki/disconnect", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("disconnecting: %d", r.Status)
	}
	if _, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "acme-wiki"); err == nil {
		t.Error("the wiki is still connected")
	}
}

func TestAServerOfTheirOwnIsCheckedBeforeItsAdded(t *testing.T) {
	w, _, wiki := customWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	for what, try := range map[string]struct{ title, address, token, says string }{
		"a token the server refuses": {"Acme Wiki", wiki.URL + "/mcp", "wrong", "didn't take the token"},
		"plain http":                 {"Acme Wiki", "http://wiki.example.com/mcp", "wiki-token", "https://"},
		"a private address":          {"Acme Wiki", "https://10.0.0.5/mcp", "wiki-token", "private"},
		"a catalogue name":           {"Linear", wiki.URL + "/mcp", "wiki-token", "already"},
		"no name":                    {"", wiki.URL + "/mcp", "wiki-token", "Name it"},
		"no sign-in and no token":    {"Acme Wiki", wiki.URL + "/mcp", "", "token"},
	} {
		r := addCustom(ada, try.title, try.address, try.token)
		if r.Status == http.StatusSeeOther || !strings.Contains(VisibleText(r), try.says) {
			t.Errorf("%s: %d, want it refused saying %q:\n%s", what, r.Status, try.says, VisibleText(r))
		}
	}
	if have, _ := w.Store.(platform.Connections).Connections(context.Background(), "acme"); len(have) != 0 {
		t.Errorf("something was added: %+v", have)
	}
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/connectors"); strings.Contains(page.Body, `action="/connectors/custom"`) {
		t.Error("bo, who isn't the owner, is offered adding a server")
	}
	if r := addCustom(bo, "Acme Wiki", wiki.URL+"/mcp", "wiki-token"); r.Status != http.StatusNotFound {
		t.Errorf("bo adding a server: %d, want 404", r.Status)
	}
}

// Kept in the database with its connection, its token sealed and bound to
// its address: one changed in the database doesn't send the token there.
func TestAServerOfTheirOwnIsKeptSealedAndBoundToItsAddress(t *testing.T) {
	f := newFakeVendor(t)
	wiki := wikiServer(t)
	w := NewWorldWithPostgres(t, withVendor(f), func(o *Setup) {
		pool := x509.NewCertPool()
		pool.AddCert(wiki.Certificate())
		o.ConnectorEgress = egress.Options{RootCAs: pool, AllowLoopback: true, PrivateHosts: []string{"127.0.0.1"}}
	})
	ada := w.Browser("acme").SignedIn("ada")
	if r := addCustom(ada, "Acme Wiki", wiki.URL+"/mcp", "wiki-token"); r.Status != http.StatusSeeOther {
		t.Fatalf("adding the wiki: %d %s", r.Status, VisibleText(r))
	}

	w.RestartPlatform()

	c, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "acme-wiki")
	if err != nil || c.Custom == nil || c.Custom.URL != wiki.URL+"/mcp" || c.Tokens.Access != "wiki-token" {
		t.Fatalf("after a restart: %+v, %v", c, err)
	}
	conn, err := pgx.Connect(context.Background(), w.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var access []byte
	if err := conn.QueryRow(context.Background(), `select access_token from connections where connector = 'acme-wiki'`).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(access), "wiki-token") {
		t.Errorf("the token is stored as it is: %q", access)
	}
	if _, err := conn.Exec(context.Background(), `update connections set custom = jsonb_set(custom, '{URL}', '"https://evil.example.com/mcp"') where connector = 'acme-wiki'`); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "acme-wiki"); err == nil {
		t.Errorf("with its address changed in the database, the token still opened: %+v", c.Tokens)
	}
}

// One with the MCP sign-in is connected as the catalogue's are.
func TestAServerOfTheirOwnWithItsOwnSignInIsConnectedThatWay(t *testing.T) {
	w, f, _ := customWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	r := addCustom(ada, "Tracker", f.srv.URL+"/mcp", "")
	m := goOn.FindStringSubmatch(r.Body)
	if r.Status != http.StatusOK || m == nil {
		t.Fatalf("adding a server with its own sign-in: %d\n%s", r.Status, VisibleText(r))
	}
	authorize := html.UnescapeString(m[1])
	u, _ := url.Parse(authorize)
	back := url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {f.srv.URL}}
	if r := ada.Get("/oauth/callback?" + back.Encode()); r.Status != http.StatusSeeOther {
		t.Fatalf("coming back: %d %s", r.Status, VisibleText(r))
	}

	c, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "tracker")
	if err != nil || c.Custom == nil || c.Custom.URL != f.srv.URL+"/mcp" || c.Tokens.Access != "at-1" || c.Tokens.Refresh != "rt-1" {
		t.Fatalf("acme's tracker: %+v, %v", c, err)
	}
	if status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/connectors/tracker/token"); status != http.StatusOK || out["token"] != "at-1" {
		t.Errorf("the runner's token: %d %v", status, out)
	}
}
