// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// GitHub for customers: the owner connects their network to the public
// GuildNet app from the home page (where they're signed in with Google as
// its owner), GitHub sends them back, and the platform checks that the
// installation is one their GitHub account can reach before binding it to
// the network. The network's runner then gets tokens for that installation,
// and only it, from the platform's broker: the app's key never leaves the
// platform.

// fakeGitHubApp is GitHub as the platform sees it: the sign-in code
// exchange, a person's installations, and minting installation tokens.
type fakeGitHubApp struct {
	srv    *httptest.Server
	mu     sync.Mutex
	users  map[string][]int64 // sign-in code -> the installations that person can reach
	minted []mintedToken
	// Installation N is on the organization acctN, whose admins are those
	// who can reach it, unless they only read: readers[code] are the
	// installations that person can only read through. personal are
	// installations on a person's own account instead, by its login.
	readers  map[string][]int64
	personal map[int64]string
	// listing is what listing an installation's repos answers with, while
	// it's failing: a status and nothing more.
	listing int
}

// loginOf is the GitHub login of the person who signed in with code.
func loginOf(code string) string { return "user-" + code }

type mintedToken struct {
	Installation string
	Repos        []string
	Permissions  map[string]string
}

func newFakeGitHubApp(t *testing.T) *fakeGitHubApp {
	f := &fakeGitHubApp{users: map[string][]int64{}, readers: map[string][]int64{}, personal: map[int64]string{}}
	mux := http.NewServeMux()
	codeOf := func(r *http.Request) (string, bool) {
		code := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "token "), "gho_")
		f.mu.Lock()
		defer f.mu.Unlock()
		_, ok := f.users[code]
		return code, ok
	}
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		code, ok := codeOf(r)
		if !ok {
			writeGH(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		writeGH(w, 200, map[string]any{"login": loginOf(code)})
	})
	// The installation's repos as this person has them: admin on all of
	// them if it's their organization (as an admin) or their own account.
	mux.HandleFunc("GET /user/installations/{id}/repositories", func(w http.ResponseWriter, r *http.Request) {
		code, ok := codeOf(r)
		if !ok {
			writeGH(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.mu.Lock()
		defer f.mu.Unlock()
		if !slices.Contains(f.users[code], id) {
			writeGH(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		admin := !slices.Contains(f.readers[code], id)
		if login, personal := f.personal[id]; personal {
			admin = login == loginOf(code)
		}
		perms := map[string]bool{"admin": admin, "push": admin, "pull": true}
		writeGH(w, 200, map[string]any{"total_count": 2, "repositories": []map[string]any{
			{"name": "web", "permissions": perms}, {"name": "docs", "permissions": perms}}})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostFormValue("client_id") != "Iv1.test" || r.PostFormValue("client_secret") != "client-secret" {
			writeGH(w, 401, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		f.mu.Lock()
		_, ok := f.users[r.PostFormValue("code")]
		f.mu.Unlock()
		if !ok {
			writeGH(w, 200, map[string]string{"error": "bad_verification_code"})
			return
		}
		writeGH(w, 200, map[string]string{"access_token": "gho_" + r.PostFormValue("code"), "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "token ")
		f.mu.Lock()
		ids, ok := f.users[strings.TrimPrefix(code, "gho_")]
		f.mu.Unlock()
		if !ok {
			writeGH(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		var list []map[string]any
		f.mu.Lock()
		for _, id := range ids {
			account := map[string]any{"login": "acct" + Itoa(id), "type": "Organization"}
			if login, ok := f.personal[id]; ok {
				account = map[string]any{"login": login, "type": "User"}
			}
			list = append(list, map[string]any{"id": id, "account": account})
		}
		f.mu.Unlock()
		writeGH(w, 200, map[string]any{"total_count": len(list), "installations": list})
	})
	mux.HandleFunc("GET /app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeGH(w, 200, map[string]any{"id": r.PathValue("id"), "account": map[string]any{"login": "acct" + r.PathValue("id")},
			"permissions": map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "checks": "read", "statuses": "read", "metadata": "read"}})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeGH(w, 401, map[string]string{"message": "an app JWT is required"})
			return
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.minted = append(f.minted, mintedToken{Installation: r.PathValue("id"), Repos: body.Repositories, Permissions: body.Permissions})
		n := len(f.minted)
		f.mu.Unlock()
		writeGH(w, 201, map[string]any{"token": "ghs_" + r.PathValue("id") + "_" + Itoa(int64(n)), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		failing := f.listing
		f.mu.Unlock()
		if failing != 0 {
			w.WriteHeader(failing)
			return
		}
		writeGH(w, 200, map[string]any{"total_count": 2, "repositories": []map[string]any{{"name": "web", "private": false}, {"name": "docs", "private": true}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHubApp) canReach(code string, installations ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[code] = installations
}

// onlyReads makes the person with code a plain member of installation's
// organization: they see it, but can't run it.
func (f *fakeGitHubApp) onlyReads(code string, installation int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readers[code] = append(f.readers[code], installation)
}

// onPersonalAccount puts installation on login's own GitHub account.
func (f *fakeGitHubApp) onPersonalAccount(installation int64, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.personal[installation] = login
}

// failListing makes listing repos answer with status, and nothing more.
func (f *fakeGitHubApp) failListing(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listing = status
}

func (f *fakeGitHubApp) lastMinted() mintedToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.minted) == 0 {
		return mintedToken{}
	}
	return f.minted[len(f.minted)-1]
}

func writeGH(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func appKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func newGitHubWorld(t *testing.T) (*World, *fakeGitHubApp) {
	t.Helper()
	gh := newFakeGitHubApp(t)
	key := appKey(t)
	s := platform.NewMemoryStore()
	w := NewWorldWith(t, func() Store { return s }, AnyoneCreates, func(o *Setup) {
		o.GitHub = &platform.GitHubApp{AppID: 1, Slug: "guildnet", ClientID: "Iv1.test", ClientSecret: "client-secret", Key: key,
			APIURL: gh.srv.URL, WebURL: gh.srv.URL}
	})
	return w, gh
}

// connect starts connecting GitHub from the home page and returns the state
// GitHub would carry back.
func connect(t *testing.T, b *Browser, network string) string {
	t.Helper()
	b.Get("/networks/" + network)
	r := b.Post("/networks/"+network+"/github", nil)
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.Status != http.StatusSeeOther || loc == nil || !strings.HasSuffix(loc.Path, "/apps/guildnet/installations/new") || loc.Query().Get("state") == "" {
		t.Fatalf("connecting: %d to %q", r.Status, r.Header.Get("Location"))
	}
	return loc.Query().Get("state")
}

func callback(b *Browser, code string, installation int64, state string) Response {
	return b.Get("/github/callback?" + url.Values{"code": {code}, "installation_id": {Itoa(installation)}, "setup_action": {"install"}, "state": {state}}.Encode())
}

func TestTheOwnerConnectsTheirNetworkToGitHub(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)
	if page := bob.Get("/networks/bobco"); !strings.Contains(page.Body, `action="/networks/bobco/github"`) {
		t.Fatalf("the network's page should offer connecting GitHub:\n%s", page.Text())
	}

	r := callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks/bobco" {
		t.Fatalf("coming back from GitHub: %d to %q %s", r.Status, r.Header.Get("Location"), r.Text())
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "bobco"); g.Installation != 7 || g.Account != "acct7" {
		t.Errorf("bobco = installation %d, account %q; want 7, acct7", g.Installation, g.Account)
	}
	if page := bob.Get("/networks/bobco"); !strings.Contains(page.Text(), "acct7") {
		t.Error("the network's page should say which GitHub account it's connected to")
	}
}

// The installation id comes back in the address, where anyone could type
// someone else's: it's only bound if the person's own GitHub account can
// reach it.
func TestAnInstallationOnlyConnectsIfTheirGitHubAccountCanReachIt(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)

	r := callback(bob, "bobs-code", 99, connect(t, bob, "bobco"))

	if r.Status != http.StatusForbidden {
		t.Errorf("someone else's installation: %d, want 403", r.Status)
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "bobco"); g.Installation != 0 {
		t.Errorf("bobco was bound to installation %d", g.Installation)
	}
}

// Seeing an installation isn't enough to connect it: GitHub lists it for
// anyone who can read one of its repos, and the network would get write to
// all of them. Only an admin of the account it's on (the organization, or
// the person whose own account it is) connects it.
func TestOnlyAnAdminOfTheGitHubAccountConnectsIt(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7, 8, 9)
	gh.onlyReads("bobs-code", 7)
	gh.onPersonalAccount(8, "someone-else")
	gh.onPersonalAccount(9, loginOf("bobs-code"))

	for _, theirs := range []int64{7, 8} {
		if r := callback(bob, "bobs-code", theirs, connect(t, bob, "bobco")); r.Status != http.StatusForbidden {
			t.Errorf("installation %d, which bob isn't an admin of: %d, want 403", theirs, r.Status)
		}
		if g, _ := w.Store.GitHubOf(context.Background(), "bobco"); g.Installation != 0 {
			t.Fatalf("bobco was bound to installation %d", g.Installation)
		}
	}
	if r := callback(bob, "bobs-code", 9, connect(t, bob, "bobco")); r.Status != http.StatusSeeOther {
		t.Errorf("bob's own account's installation: %d %s", r.Status, r.Text())
	}
}

// Coming back from GitHub only works in the browser, signed in as the
// network's owner, that started connecting: otherwise someone could start
// connecting their own network and get someone else to finish it with
// their GitHub account.
func TestComingBackFromGitHubOnlyWorksWhereConnectingStarted(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	carol := w.CreateNetwork("carol@example.com", "carolco")
	gh.canReach("carols-code", 8)
	state := connect(t, bob, "bobco")

	if r := callback(carol, "carols-code", 8, state); r.Status != http.StatusBadRequest {
		t.Errorf("carol finishing bob's connect: %d, want 400", r.Status)
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "bobco"); g.Installation != 0 {
		t.Errorf("bobco was bound to carol's installation %d", g.Installation)
	}
}

func TestAnInstallationIsOnlyEverForOneNetwork(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	carol := w.CreateNetwork("carol@example.com", "carolco")
	gh.canReach("bobs-code", 7)
	gh.canReach("carols-code", 7) // both can reach it: say, the same GitHub organization
	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))

	if r := callback(carol, "carols-code", 7, connect(t, carol, "carolco")); r.Status != http.StatusConflict {
		t.Errorf("binding bobco's installation to carolco too: %d, want 409", r.Status)
	}
}

func TestTheOwnerDisconnectsGitHub(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)
	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))
	bob.Get("/networks/bobco")

	if r := bob.Post("/networks/bobco/github/disconnect", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("disconnecting: %d", r.Status)
	}
	if g, _ := w.Store.GitHubOf(context.Background(), "bobco"); g.Installation != 0 {
		t.Errorf("bobco is still bound to %d", g.Installation)
	}
}

// The broker: a network's runner asks, with its network's secret, for a
// token for some of its repos. It only ever gets tokens for its own
// network's installation.
// Runners ask which repos their network has every couple of minutes; GitHub
// is asked every few minutes at most, not each time.
func TestTheRepoListIsntFetchedFromGitHubEachTime(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)
	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	n, _ := w.Store.Network(context.Background(), "bobco")
	list := func() int {
		req, _ := http.NewRequest("GET", broker.URL+"/github", nil)
		req.Header.Set("Authorization", "Bearer "+n.RunnerToken)
		req.Header.Set("GuildNet-Network", "bobco")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for range 3 {
		if status := list(); status != http.StatusOK {
			t.Fatalf("listing bobco's repos: %d", status)
		}
	}
	gh.mu.Lock()
	minted := len(gh.minted)
	gh.mu.Unlock()
	if minted != 1 {
		t.Errorf("GitHub minted %d tokens for three lists, want 1", minted)
	}
}

// GitHub sometimes fails to list an installation's repos (a 500 with
// nothing in it). The broker gives the runner the list it last had rather
// than failing it.
func TestABriefGitHubFailureGivesTheRepoListLastHad(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)
	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))
	secret := RunnerSecret(w, "bobco")
	if status, out := BrokerCall(t, w, "bobco", secret, "GET", "/github"); status != http.StatusOK {
		t.Fatalf("listing bobco's repos: %d %v", status, out)
	}

	gh.failListing(http.StatusInternalServerError)
	w.Clock.Advance(10 * time.Minute)
	status, out := BrokerCall(t, w, "bobco", secret, "GET", "/github")

	if repos, _ := out["repos"].([]any); status != http.StatusOK || len(repos) != 2 {
		t.Errorf("while GitHub fails: %d %v, want web and docs, as last listed", status, out)
	}
}

func TestTheBrokerGivesARunnerTokensForItsOwnInstallationOnly(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	w.CreateNetwork("carol@example.com", "carolco")
	gh.canReach("bobs-code", 7)
	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	secret := func(name string) string {
		n, _ := w.Store.Network(context.Background(), name)
		return n.RunnerToken
	}
	ask := func(network, token, path string, body any) (int, map[string]any) {
		var in bytes.Buffer
		method := "GET"
		if body != nil {
			method = "POST"
			json.NewEncoder(&in).Encode(body)
		}
		req, _ := http.NewRequest(method, broker.URL+path, &in)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("GuildNet-Network", network)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	status, out := ask("bobco", secret("bobco"), "/github/token", map[string]any{"repos": []string{"web"}, "write": false})
	if status != http.StatusOK || out["token"] == "" {
		t.Fatalf("bobco's runner asking: %d %v", status, out)
	}
	if m := gh.lastMinted(); m.Installation != "7" || !slices.Equal(m.Repos, []string{"web"}) || m.Permissions["contents"] != "read" {
		t.Errorf("minted %+v, want a read-only token for web on installation 7", m)
	}
	if status, _ := ask("bobco", secret("carolco"), "/github/token", map[string]any{"repos": []string{"web"}}); status != http.StatusUnauthorized {
		t.Errorf("asking for bobco with carolco's secret: %d, want 401", status)
	}
	if status, _ := ask("carolco", secret("carolco"), "/github/token", map[string]any{"repos": []string{"web"}}); status != http.StatusNotFound {
		t.Errorf("carolco, with no GitHub: %d, want 404", status)
	}
	status, out = ask("bobco", secret("bobco"), "/github", nil)
	if status != http.StatusOK || out["account"] != "acct7" || len(out["repos"].([]any)) != 2 {
		t.Errorf("bobco's GitHub: %d %v, want acct7 with web and docs", status, out)
	}

	// A network that's suspended, or being deleted, gets nothing more.
	for _, st := range []string{platform.Suspended, platform.Deleting} {
		n, _ := w.Store.Network(context.Background(), "bobco")
		n.Status = st
		w.Store.UpdateNetwork(context.Background(), n)
		if status, _ := ask("bobco", secret("bobco"), "/github/token", map[string]any{"repos": []string{"web"}}); status != http.StatusUnauthorized {
			t.Errorf("bobco %s, asking for a token: %d, want 401", st, status)
		}
	}
}

// Browsers hold a form's redirect to the page's form-action policy, so the
// network's page lets "Connect GitHub" go on to GitHub.
func TestTheNetworksPageLetsConnectingGoOnToGitHub(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")

	policy := bob.Get("/networks/bobco").Header.Get("Content-Security-Policy")

	if !strings.Contains(policy, "form-action 'self' ") || !strings.Contains(policy, gh.srv.URL) {
		t.Errorf("the network page's policy %q should let its forms go on to GitHub (%s)", policy, gh.srv.URL)
	}
}

// The runner's told which of its repos are public: what's in them may have
// been written by anyone, so they're a source of their own for its agents.
func TestTheBrokerSaysWhichReposArePublic(t *testing.T) {
	w, gh := newGitHubWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	gh.canReach("bobs-code", 7)
	callback(bob, "bobs-code", 7, connect(t, bob, "bobco"))

	status, out := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "GET", "/github")

	if status != http.StatusOK || fmt.Sprint(out["repos"]) != "[docs web]" || fmt.Sprint(out["public"]) != "[web]" {
		t.Errorf("the broker says %d %v: want repos web and docs, web public", status, out)
	}
	// And the App's bot, which opens the agents' PRs: what makes one theirs.
	if out["bot"] != "guildnet[bot]" {
		t.Errorf("the broker says the bot is %v, want guildnet[bot]", out["bot"])
	}
}
