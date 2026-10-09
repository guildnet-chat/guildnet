// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A network's owner connects GitHub and services from the network's own
// console, signed in with their IRC account: a network set up without the
// home page (as one is when you run GuildNet yourself) has no owner there.
// Each service sends people back to one address, on the home page, which
// passes them on to the console they started from; that's where it's
// checked that this browser started it. acme's owner is ada.

// connectFromConsole presses Connect for connector on the console and
// returns where the owner is sent.
func connectFromConsole(t *testing.T, b *Browser, connector string) string {
	t.Helper()
	b.Get("/connectors")
	r := b.Post("/connectors/"+connector+"/connect", nil)
	m := goOn.FindStringSubmatch(r.Body)
	if r.Status != http.StatusOK || m == nil {
		t.Fatalf("connecting %s from the console: %d\n%s", connector, r.Status, r.Text())
	}
	return html.UnescapeString(m[1])
}

// passedOn checks the home page's callback passes path?q on to acme's
// console.
func passedOn(t *testing.T, w *World, path string, q url.Values) {
	t.Helper()
	r := w.Home().Get(path + "?" + q.Encode())
	if want := "http://acme." + BaseDomain + path + "?" + q.Encode(); r.Status != http.StatusSeeOther || r.Header.Get("Location") != want {
		t.Fatalf("the home page's %s: %d to %q, want on to %q\n%s", path, r.Status, r.Header.Get("Location"), want, r.Text())
	}
}

func TestTheOwnerConnectsAServiceFromTheConsole(t *testing.T) {
	w, f := newConnectorWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	if page := ada.Get("/connectors"); !strings.Contains(page.Body, `action="/connectors/linear/connect"`) {
		t.Fatalf("the console's connectors page should offer the owner connecting Linear:\n%s", page.Text())
	}

	authorize := connectFromConsole(t, ada, "linear")
	u, _ := url.Parse(authorize)
	if u.Query().Get("redirect_uri") != "http://"+BaseDomain+"/oauth/callback" {
		t.Errorf("redirect_uri %q, want the one address every network comes back to", u.Query().Get("redirect_uri"))
	}
	back := url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {f.srv.URL}}
	passedOn(t, w, "/oauth/callback", back)
	r := ada.Get("/oauth/callback?" + back.Encode())

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/connectors" {
		t.Fatalf("coming back to the console: %d to %q\n%s", r.Status, r.Header.Get("Location"), r.Text())
	}
	c, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "linear")
	if err != nil || c.Tokens.Access != "at-1" {
		t.Fatalf("acme's Linear: %+v, %v; want the tokens the service gave", c.Tokens, err)
	}
	if status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/connectors/linear/token"); status != http.StatusOK || out["token"] != "at-1" {
		t.Errorf("the runner's token: %d %v", status, out)
	}

	if page := ada.Get("/connectors"); !strings.Contains(page.Body, `action="/connectors/linear/disconnect"`) {
		t.Fatalf("the page should say Linear is connected, with a way to disconnect:\n%s", page.Text())
	}
	if r := ada.Post("/connectors/linear/disconnect", nil); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/connectors" {
		t.Errorf("disconnecting: %d to %q", r.Status, r.Header.Get("Location"))
	}
	if _, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "linear"); err == nil {
		t.Error("acme is still connected to Linear")
	}
	if !slices.Contains(f.revokedTokens(), "rt-1") {
		t.Errorf("the service was asked to forget %v, want rt-1", f.revokedTokens())
	}
}

// Only the owner, and only in the browser that started: otherwise someone
// could start connecting and get someone else to finish it, as them.
func TestOnlyTheOwnerConnectsFromTheConsoleAndOnlyWhereTheyStarted(t *testing.T) {
	w, f := newConnectorWorld(t)
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/connectors"); strings.Contains(page.Body, `action="/connectors/linear/connect"`) || strings.Contains(page.Body, `action="/github/connect"`) {
		t.Errorf("bo isn't the owner, but is offered connecting:\n%s", page.Text())
	}
	if r := bo.Post("/connectors/linear/connect", nil); r.Status != http.StatusNotFound {
		t.Errorf("bo connecting: %d, want 404", r.Status)
	}

	ada := w.Browser("acme").SignedIn("ada")
	authorize := connectFromConsole(t, ada, "linear")
	u, _ := url.Parse(authorize)
	back := url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {f.srv.URL}}

	if r := bo.Get("/oauth/callback?" + back.Encode()); r.Status == http.StatusSeeOther {
		t.Errorf("bo finished ada's connect: %d to %q", r.Status, r.Header.Get("Location"))
	}
	if r := w.Browser("acme").SignedIn("ada").Get("/oauth/callback?" + back.Encode()); r.Status == http.StatusSeeOther {
		t.Errorf("another of ada's browsers finished the connect: %d to %q", r.Status, r.Header.Get("Location"))
	}
	if _, err := w.Store.(platform.Connections).Connection(context.Background(), "acme", "linear"); err == nil {
		t.Fatal("acme was connected by someone who didn't start it")
	}
	// The browser that started it still can.
	if r := ada.Get("/oauth/callback?" + back.Encode()); r.Status != http.StatusSeeOther {
		t.Errorf("ada coming back: %d\n%s", r.Status, r.Text())
	}
}

func TestTheOwnerConnectsGitHubFromTheConsole(t *testing.T) {
	w, gh := newGitHubWorld(t)
	gh.canReach("adas-code", 7)
	ada := w.Browser("acme").SignedIn("ada")
	page := ada.Get("/connectors")
	if !strings.Contains(page.Body, `action="/github/connect"`) {
		t.Fatalf("the console's connectors page should offer the owner connecting GitHub:\n%s", page.Text())
	}
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' "+gh.srv.URL) {
		t.Errorf("CSP %q: the Connect GitHub form has to be let go on to GitHub", csp)
	}

	r := ada.Post("/github/connect", nil)
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.Status != http.StatusSeeOther || loc == nil || !strings.HasSuffix(loc.Path, "/apps/guildnet/installations/new") || loc.Query().Get("state") == "" {
		t.Fatalf("connecting: %d to %q", r.Status, r.Header.Get("Location"))
	}
	back := url.Values{"code": {"adas-code"}, "installation_id": {"7"}, "setup_action": {"install"}, "state": {loc.Query().Get("state")}}
	passedOn(t, w, "/github/callback", back)
	if r := w.Browser("acme").SignedIn("bo").Get("/github/callback?" + back.Encode()); r.Status == http.StatusSeeOther {
		t.Errorf("bo finished ada's connect: %d to %q", r.Status, r.Header.Get("Location"))
	}
	r = ada.Get("/github/callback?" + back.Encode())

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/connectors" {
		t.Fatalf("coming back to the console: %d to %q\n%s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "acme"); g.Installation != 7 || g.Account != "acct7" {
		t.Fatalf("acme = installation %d, account %q; want 7, acct7", g.Installation, g.Account)
	}
	if status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "GET", "/github"); status != http.StatusOK || out["account"] != "acct7" {
		t.Errorf("the runner asking the broker: %d %v", status, out)
	}
	page = ada.Get("/connectors")
	if !strings.Contains(page.Text(), "acct7") || !strings.Contains(page.Body, `action="/github/disconnect"`) {
		t.Fatalf("the page should say which account, with a way to disconnect:\n%s", page.Text())
	}
	if bo := w.Browser("acme").SignedIn("bo").Get("/connectors"); !strings.Contains(bo.Text(), "acct7") || strings.Contains(bo.Body, `action="/github/disconnect"`) {
		t.Errorf("bo should see it's connected, without a way to disconnect:\n%s", bo.Text())
	}
	if r := ada.Post("/github/disconnect", nil); r.Status != http.StatusSeeOther {
		t.Errorf("disconnecting: %d", r.Status)
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "acme"); g.Installation != 0 {
		t.Errorf("acme is still connected to installation %d", g.Installation)
	}
}

// Someone else's installation, named in the address, isn't connected from
// the console either.
func TestAConsoleConnectOnlyTakesAnInstallationTheOwnerCanReach(t *testing.T) {
	w, gh := newGitHubWorld(t)
	gh.canReach("adas-code", 7)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/connectors")
	loc, _ := url.Parse(ada.Post("/github/connect", nil).Header.Get("Location"))

	r := ada.Get("/github/callback?" + url.Values{"code": {"adas-code"}, "installation_id": {"99"}, "state": {loc.Query().Get("state")}}.Encode())

	if r.Status != http.StatusForbidden {
		t.Errorf("someone else's installation: %d, want 403", r.Status)
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "acme"); g.Installation != 0 {
		t.Errorf("acme was bound to installation %d", g.Installation)
	}
}
