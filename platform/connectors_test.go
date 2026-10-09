// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/platform"
)

// Connectors: a network's owner connects a service (Linear, say) from the
// network's page on the home page, where they're signed in with Google as
// its owner. The platform finds the service's authorization server from its
// MCP server, registers with it, and sends the owner there with PKCE; they
// say yes and come back, and the platform swaps the code for tokens, which
// it keeps sealed. Agents never see them.

// fakeVendor is a service's MCP server and its authorization server, as
// Linear runs them: the MCP server says where its resource metadata is, and
// the authorization server takes dynamic registration (and, if metadataDocs,
// client ID metadata documents).
type fakeVendor struct {
	srv          *httptest.Server
	metadataDocs bool
	noMetadata   bool // publishes no resource metadata, as Atlassian's
	mu           sync.Mutex
	registered   []map[string]any
	clients      map[string]string // client_id -> secret
	codes        map[string]url.Values
	issued       int
	live         map[string]bool // refresh tokens that still work
	refreshes    int
	refuse       bool // refuses every refresh token, as when someone revokes GuildNet's access
	revoked      []string
}

func newFakeVendor(t *testing.T) *fakeVendor {
	f := &fakeVendor{clients: map[string]string{}, codes: map[string]url.Values{}, live: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		if f.noMetadata {
			w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth", error="invalid_token"`)
		} else {
			w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth", resource_metadata="`+f.srv.URL+`/.well-known/oauth-protected-resource/mcp", scope="read write"`)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		if f.noMetadata {
			http.NotFound(w, r)
			return
		}
		writeGH(w, 200, map[string]any{"resource": f.srv.URL + "/mcp", "authorization_servers": []string{f.srv.URL}, "scopes_supported": []string{"read", "write"}})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeGH(w, 200, map[string]any{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"registration_endpoint": f.srv.URL + "/register", "revocation_endpoint": f.srv.URL + "/revoke", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"client_secret_basic", "none"},
			"client_id_metadata_document_supported": f.metadataDocs, "authorization_response_iss_parameter_supported": true})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.registered = append(f.registered, in)
		id := fmt.Sprintf("client-%d", len(f.registered))
		f.clients[id] = "secret-" + id
		in["client_id"], in["client_secret"] = id, "secret-"+id
		writeGH(w, 201, in)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		id, secret, basic := r.BasicAuth()
		if basic {
			id, _ = url.QueryUnescape(id)
			secret, _ = url.QueryUnescape(secret)
		} else {
			id = r.PostFormValue("client_id")
		}
		if want, confidential := f.clients[id]; confidential && secret != want {
			writeGH(w, 401, map[string]string{"error": "invalid_client"})
			return
		}
		switch r.PostFormValue("grant_type") {
		case "authorization_code":
			asked, ok := f.codes[r.PostFormValue("code")]
			delete(f.codes, r.PostFormValue("code"))
			sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
			if !ok || asked.Get("client_id") != id || asked.Get("redirect_uri") != r.PostFormValue("redirect_uri") ||
				asked.Get("resource") != r.PostFormValue("resource") ||
				asked.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
				writeGH(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			// Each refresh token works once (rotation), as Notion's.
			rt := r.PostFormValue("refresh_token")
			if f.refuse || !f.live[rt] || r.PostFormValue("resource") != f.srv.URL+"/mcp" {
				writeGH(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
			delete(f.live, rt)
			f.refreshes++
		default:
			writeGH(w, 400, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		f.issued++
		f.live[fmt.Sprintf("rt-%d", f.issued)] = true
		writeGH(w, 200, map[string]any{"access_token": fmt.Sprintf("at-%d", f.issued), "refresh_token": fmt.Sprintf("rt-%d", f.issued),
			"token_type": "Bearer", "expires_in": 3600})
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revoked = append(f.revoked, r.PostFormValue("token"))
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// approve is the owner saying yes on the service's own page: the code it
// sends them back with.
func (f *fakeVendor) approve(t *testing.T, authorize string) string {
	t.Helper()
	u, err := url.Parse(authorize)
	if err != nil || !strings.HasPrefix(authorize, f.srv.URL+"/authorize?") {
		t.Fatalf("the owner was sent to %q, not the service's sign-in", authorize)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("sign-in without PKCE or state: %v", q)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code := fmt.Sprintf("code-%d", len(f.codes)+f.issued+1)
	f.codes[code] = q
	return code
}

func (f *fakeVendor) registrations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registered)
}

func (f *fakeVendor) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

// withVendor offers f's service as "linear", reached as the platform reaches
// services (public HTTPS only), trusting f's test certificate.
func withVendor(f *fakeVendor, also ...connectors.Connector) func(*Setup) {
	return func(o *Setup) {
		pool := x509.NewCertPool()
		pool.AddCert(f.srv.Certificate())
		o.ConnectorEgress = egress.Options{RootCAs: pool, AllowLoopback: true}
		o.Connectors = append([]connectors.Connector{{ID: "linear", Title: "Linear", About: "Issues", URL: f.srv.URL + "/mcp"}}, also...)
	}
}

func newConnectorWorld(t *testing.T, tweaks ...func(*Setup)) (*World, *fakeVendor) {
	t.Helper()
	f := newFakeVendor(t)
	s := platform.NewMemoryStore()
	return NewWorldWith(t, func() Store { return s }, append([]func(*Setup){withVendor(f), AnyoneCreates}, tweaks...)...), f
}

var goOn = regexp.MustCompile(`http-equiv="refresh" content="0; url=([^"]+)"`)

// startConnecting presses Connect on the network's page and returns where
// the owner is sent.
func startConnecting(t *testing.T, b *Browser, network, connector string) string {
	t.Helper()
	b.Get("/networks/" + network)
	r := b.Post("/networks/"+network+"/connectors/"+connector, nil)
	m := goOn.FindStringSubmatch(r.Body)
	if r.Status != http.StatusOK || m == nil {
		t.Fatalf("connecting %s: %d\n%s", connector, r.Status, r.Text())
	}
	return html.UnescapeString(m[1])
}

func comeBack(b *Browser, q url.Values) Response {
	return b.Get("/oauth/callback?" + q.Encode())
}

// connectLinear connects network to the fake service, as its owner b.
func connectLinear(t *testing.T, b *Browser, f *fakeVendor, network string) {
	t.Helper()
	authorize := startConnecting(t, b, network, "linear")
	u, _ := url.Parse(authorize)
	r := comeBack(b, url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {f.srv.URL}})
	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks/"+network {
		t.Fatalf("coming back from the service: %d to %q\n%s", r.Status, r.Header.Get("Location"), r.Text())
	}
}

func TestTheOwnerConnectsAService(t *testing.T) {
	w, f := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	if page := bob.Get("/networks/bobco"); !strings.Contains(page.Body, `action="/networks/bobco/connectors/linear"`) {
		t.Fatalf("the network's page should offer connecting Linear:\n%s", page.Text())
	}

	authorize := startConnecting(t, bob, "bobco", "linear")

	q, _ := url.Parse(authorize)
	if q.Query().Get("resource") != f.srv.URL+"/mcp" || q.Query().Get("redirect_uri") != "http://"+BaseDomain+"/oauth/callback" || q.Query().Get("scope") != "read write" {
		t.Errorf("sent to sign in with %v; want the MCP server as the resource, GuildNet's callback, and the scope it asked for", q.Query())
	}
	r := comeBack(bob, url.Values{"code": {f.approve(t, authorize)}, "state": {q.Query().Get("state")}, "iss": {f.srv.URL}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("coming back: %d %s", r.Status, r.Text())
	}
	c, err := w.Store.(platform.Connections).Connection(context.Background(), "bobco", "linear")
	if err != nil || c.Tokens.Access != "at-1" || c.Tokens.Refresh != "rt-1" || c.Tokens.Expires.IsZero() {
		t.Fatalf("bobco's Linear: %+v, %v; want the tokens the service gave", c.Tokens, err)
	}
	if f.registrations() != 1 {
		t.Errorf("registered %d times, want once", f.registrations())
	}
	if page := bob.Get("/networks/bobco"); !strings.Contains(page.Body, `action="/networks/bobco/connectors/linear/disconnect"`) {
		t.Errorf("the page should say Linear is connected, with a way to disconnect:\n%s", page.Text())
	}
}

// Where the service takes client ID metadata documents, GuildNet's
// document is its client ID: nothing to register.
func TestConnectingUsesGuildNetsClientMetadataDocumentWhereTaken(t *testing.T) {
	const doc = "https://" + BaseDomain + "/oauth/client-metadata.json"
	w, f := newConnectorWorld(t, func(o *Setup) { o.ClientMetadataURL = doc })
	f.metadataDocs = true
	bob := w.CreateNetwork("bob@example.com", "bobco")

	authorize := startConnecting(t, bob, "bobco", "linear")

	if u, _ := url.Parse(authorize); u.Query().Get("client_id") != doc {
		t.Errorf("client_id %q, want the metadata document %q", u.Query().Get("client_id"), doc)
	}
	if f.registrations() != 0 {
		t.Errorf("registered %d times; the document should do", f.registrations())
	}
	var got struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
		Method       string   `json:"token_endpoint_auth_method"`
	}
	r := w.Home().Get("/oauth/client-metadata.json")
	if json.Unmarshal([]byte(r.Body), &got); r.Status != http.StatusOK || got.ClientID != doc || len(got.RedirectURIs) != 1 || got.Method != "none" {
		t.Errorf("the metadata document: %d %s", r.Status, r.Body)
	}
}

// GuildNet registers once with each service, not once per network.
func TestARegistrationIsReusedAcrossNetworks(t *testing.T) {
	w, f := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	carol := w.CreateNetwork("carol@example.com", "carolco")

	connectLinear(t, bob, f, "bobco")
	connectLinear(t, carol, f, "carolco")

	if f.registrations() != 1 {
		t.Errorf("registered %d times, want once", f.registrations())
	}
}

// Coming back only works in the browser, signed in as the network's owner,
// that started connecting: otherwise someone could get the owner to finish
// connecting someone else's network, or the other way round.
func TestComingBackOnlyWorksWhereConnectingStarted(t *testing.T) {
	w, f := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	carol := w.CreateNetwork("carol@example.com", "carolco")
	authorize := startConnecting(t, bob, "bobco", "linear")
	u, _ := url.Parse(authorize)

	r := comeBack(carol, url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {f.srv.URL}})

	if r.Status != http.StatusBadRequest {
		t.Errorf("carol finishing bob's connect: %d, want 400", r.Status)
	}
	for _, n := range []string{"bobco", "carolco"} {
		if _, err := w.Store.(platform.Connections).Connection(context.Background(), n, "linear"); err == nil {
			t.Errorf("%s was connected", n)
		}
	}
}

// A server that names itself when sending the owner back must name itself:
// otherwise the code could be one another server issued (a mix-up).
func TestComingBackFromTheWrongIssuerIsRefused(t *testing.T) {
	w, f := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	for _, iss := range []string{"https://evil.example", ""} {
		authorize := startConnecting(t, bob, "bobco", "linear")
		u, _ := url.Parse(authorize)

		r := comeBack(bob, url.Values{"code": {f.approve(t, authorize)}, "state": {u.Query().Get("state")}, "iss": {iss}})

		if r.Status != http.StatusBadRequest {
			t.Errorf("coming back with iss %q: %d, want 400", iss, r.Status)
		}
	}
	if _, err := w.Store.(platform.Connections).Connection(context.Background(), "bobco", "linear"); err == nil {
		t.Error("bobco was connected")
	}
}

func TestSayingNoAtTheServiceConnectsNothing(t *testing.T) {
	w, _ := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	u, _ := url.Parse(startConnecting(t, bob, "bobco", "linear"))

	r := comeBack(bob, url.Values{"error": {"access_denied"}, "state": {u.Query().Get("state")}})

	if !strings.Contains(r.Text(), "Linear wasn't connected") {
		t.Errorf("the owner should be told: %d\n%s", r.Status, r.Text())
	}
	if _, err := w.Store.(platform.Connections).Connection(context.Background(), "bobco", "linear"); err == nil {
		t.Error("bobco was connected")
	}
}

func TestOnlyTheOwnerConnectsTheirNetwork(t *testing.T) {
	w, _ := newConnectorWorld(t)
	w.CreateNetwork("bob@example.com", "bobco")
	carol := w.CreateNetwork("carol@example.com", "carolco")
	carol.Get("/networks/carolco")

	if r := carol.Post("/networks/bobco/connectors/linear", nil); r.Status != http.StatusNotFound {
		t.Errorf("carol connecting bobco: %d, want 404", r.Status)
	}
}

// Disconnecting forgets the tokens, and asks the service to forget them too.
func TestTheOwnerDisconnectsAService(t *testing.T) {
	out := &Outbox{}
	w, f := newConnectorWorld(t, WithMail(out))
	bob := w.CreateNetwork("bob@example.com", "bobco")
	connectLinear(t, bob, f, "bobco")
	bob.Get("/networks/bobco")

	if r := bob.Post("/networks/bobco/connectors/linear/disconnect", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("disconnecting: %d %s", r.Status, r.Text())
	}

	if _, err := w.Store.(platform.Connections).Connection(context.Background(), "bobco", "linear"); err == nil {
		t.Error("bobco is still connected")
	}
	if got := f.revokedTokens(); len(got) != 1 || got[0] != "rt-1" {
		t.Errorf("revoked %v, want the refresh token", got)
	}
	var subjects []string
	for _, m := range out.To("bob@example.com") {
		subjects = append(subjects, m.Subject)
	}
	if s := strings.Join(subjects, "\n"); !strings.Contains(s, "bobco was connected to Linear") || !strings.Contains(s, "bobco was disconnected from Linear") {
		t.Errorf("bob should get security alerts for both; got:\n%s", s)
	}
}

// Some servers (Atlassian's) don't say where their authorization server is;
// the catalogue does.
func TestAServerWithoutResourceMetadataUsesTheCataloguesIssuer(t *testing.T) {
	f := newFakeVendor(t)
	f.noMetadata = true
	s := platform.NewMemoryStore()
	w := NewWorldWith(t, func() Store { return s }, withVendor(f), func(o *Setup) {
		o.Connectors = []connectors.Connector{{ID: "atlassian", Title: "Atlassian", URL: f.srv.URL + "/v1/mcp", Issuer: f.srv.URL}}
	})
	bob := w.CreateNetwork("bob@example.com", "bobco")

	authorize := startConnecting(t, bob, "bobco", "atlassian")

	if u, _ := url.Parse(authorize); u.Query().Get("resource") != f.srv.URL+"/v1/mcp" {
		t.Errorf("resource %q, want the MCP server", u.Query().Get("resource"))
	}
}

// Tokens are sealed in the platform's database, and kept across restarts.
func TestConnectionsAreKeptSealed(t *testing.T) {
	f := newFakeVendor(t)
	w := NewWorldWithPostgres(t, withVendor(f))
	bob := w.CreateNetwork("bob@example.com", "bobco")
	connectLinear(t, bob, f, "bobco")

	w.RestartPlatform()

	if page := bob.Get("/networks/bobco"); !strings.Contains(page.Body, `action="/networks/bobco/connectors/linear/disconnect"`) {
		t.Errorf("after a restart, Linear should still be connected:\n%s", page.Text())
	}
	c, err := w.Store.(platform.Connections).Connection(context.Background(), "bobco", "linear")
	if err != nil || c.Tokens.Access != "at-1" || c.Tokens.Refresh != "rt-1" || c.Client.Secret == "" {
		t.Fatalf("after a restart: %+v, %v", c, err)
	}
	conn, err := pgx.Connect(context.Background(), w.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var access, refresh, secret []byte
	if err := conn.QueryRow(context.Background(), `select access_token, refresh_token, client_secret from connections`).Scan(&access, &refresh, &secret); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{access, refresh, secret} {
		if strings.Contains(string(b), "at-1") || strings.Contains(string(b), "rt-1") || strings.Contains(string(b), "secret-client") {
			t.Errorf("a token is stored as it is: %q", b)
		}
	}
}

// The broker: a network's runner asks, with its network's secret, for an
// access token for one of its connections. It only ever gets its own
// network's, and never a refresh token.

func TestTheBrokerGivesARunnerAccessTokensForItsOwnConnections(t *testing.T) {
	w, f := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	w.CreateNetwork("carol@example.com", "carolco")
	connectLinear(t, bob, f, "bobco")

	status, out := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "GET", "/connectors")
	list, _ := out["connectors"].([]any)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("bobco's connectors: %d %v", status, out)
	}
	if c := list[0].(map[string]any); c["id"] != "linear" || c["url"] != f.srv.URL+"/mcp" || c["reconnect"] != false {
		t.Errorf("bobco's Linear: %v", c)
	}
	status, out = BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "POST", "/connectors/linear/token")
	if status != http.StatusOK || out["token"] != "at-1" || out["expires_at"] == nil {
		t.Errorf("bobco's Linear token: %d %v", status, out)
	}
	for k := range out {
		if strings.Contains(k, "refresh") {
			t.Errorf("the runner was given %q", k)
		}
	}
	if status, _ := BrokerCall(t, w, "bobco", RunnerSecret(w, "carolco"), "POST", "/connectors/linear/token"); status != http.StatusUnauthorized {
		t.Errorf("asking for bobco's with carolco's secret: %d, want 401", status)
	}
	if status, _ := BrokerCall(t, w, "carolco", RunnerSecret(w, "carolco"), "POST", "/connectors/linear/token"); status != http.StatusNotFound {
		t.Errorf("carolco, not connected: %d, want 404", status)
	}
}

// A token near its end is refreshed first, and the new refresh token kept:
// some services (Notion) give a new one each time, and the old one stops
// working.
func TestAnEndingTokenIsRefreshedFirst(t *testing.T) {
	w, f := newConnectorWorld(t)
	bob := w.CreateNetwork("bob@example.com", "bobco")
	connectLinear(t, bob, f, "bobco")
	w.Clock.Advance(58 * time.Minute)

	status, out := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "POST", "/connectors/linear/token")

	if status != http.StatusOK || out["token"] != "at-2" {
		t.Fatalf("with 2 minutes left: %d %v, want a fresh token", status, out)
	}
	if c, _ := w.Store.(platform.Connections).Connection(context.Background(), "bobco", "linear"); c.Tokens.Refresh != "rt-2" {
		t.Errorf("kept refresh token %q, want the new one", c.Tokens.Refresh)
	}
	w.Clock.Advance(58 * time.Minute)
	if _, out := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "POST", "/connectors/linear/token"); out["token"] != "at-3" {
		t.Errorf("refreshing again, with the kept refresh token: %v", out)
	}
}

// Two calls at once refresh once: with rotation, a second refresh with the
// same refresh token would be refused, and the connection lost.
func TestCallsAtOnceRefreshOnce(t *testing.T) {
	for name, open := range map[string]func(t *testing.T, f *fakeVendor) *World{
		"memory": func(t *testing.T, f *fakeVendor) *World {
			s := platform.NewMemoryStore()
			return NewWorldWith(t, func() Store { return s }, withVendor(f))
		},
		"postgres": func(t *testing.T, f *fakeVendor) *World { return NewWorldWithPostgres(t, withVendor(f)) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeVendor(t)
			w := open(t, f)
			bob := w.CreateNetwork("bob@example.com", "bobco")
			connectLinear(t, bob, f, "bobco")
			w.Clock.Advance(58 * time.Minute)
			var wg sync.WaitGroup
			tokens := make([]any, 8)
			for i := range tokens {
				wg.Go(func() {
					_, out := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "POST", "/connectors/linear/token")
					tokens[i] = out["token"]
				})
			}
			wg.Wait()

			for i, tok := range tokens {
				if tok != "at-2" {
					t.Errorf("call %d got %v, want at-2", i, tok)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.refreshes != 1 {
				t.Errorf("refreshed %d times, want once", f.refreshes)
			}
		})
	}
}

// When the service refuses the refresh token (someone took GuildNet's
// access away there), the connection needs the owner to connect again: the
// runner is told, and the owner emailed, once.
func TestARefusedRefreshAsksTheOwnerToReconnect(t *testing.T) {
	out := &Outbox{}
	w, f := newConnectorWorld(t, WithMail(out))
	bob := w.CreateNetwork("bob@example.com", "bobco")
	connectLinear(t, bob, f, "bobco")
	f.mu.Lock()
	f.refuse = true
	f.mu.Unlock()
	w.Clock.Advance(time.Hour)

	for range 2 {
		if status, _ := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "POST", "/connectors/linear/token"); status != http.StatusConflict {
			t.Errorf("a refused refresh: %d, want 409", status)
		}
	}

	if _, got := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "GET", "/connectors"); !strings.Contains(fmt.Sprint(got), "reconnect:true") {
		t.Errorf("the runner should hear it needs reconnecting: %v", got)
	}
	var told int
	for _, m := range out.To("bob@example.com") {
		if strings.Contains(m.Subject, "reconnect") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("bob was told %d times, want once", told)
	}
	page := bob.Get("/networks/bobco")
	if !strings.Contains(page.Text(), "Needs reconnecting") || !strings.Contains(page.Body, `action="/networks/bobco/connectors/linear"`) {
		t.Errorf("the page should say so, and offer to connect again:\n%s", page.Text())
	}
	f.mu.Lock()
	f.refuse = false
	f.mu.Unlock()
	connectLinear(t, bob, f, "bobco")
	if status, _ := BrokerCall(t, w, "bobco", RunnerSecret(w, "bobco"), "POST", "/connectors/linear/token"); status != http.StatusOK {
		t.Errorf("after connecting again: %d", status)
	}
}

// In the console: a team's leads choose which connected services the team
// uses, and approvers set what each tool may do and approve changed ones.

// consoleConnectors is the network's connected services, as its runner
// sees them.
type consoleConnectors struct {
	mu  sync.Mutex
	set []string
}

func (c *consoleConnectors) Services() []connectors.Service {
	return []connectors.Service{
		{ID: "linear", Title: "Linear", Tools: []connectors.ToolState{
			{Name: "create_issue", Description: "Create an issue, and email it", Default: "ask", State: "changed"},
			{Name: "search_issues", Description: "Search issues", Default: "allow", State: "approved"},
		}},
		{ID: "sentry", Title: "Sentry", Tools: []connectors.ToolState{
			{Name: "execute_sentry_tool", Description: "Run any Sentry tool by name", Default: "reads", State: "approved"},
		}},
		{ID: "notion", Title: "Notion", Reconnect: true},
	}
}

func (c *consoleConnectors) SetPolicy(_ context.Context, connector, tool, policy string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = append(c.set, connector+" "+tool+" policy "+policy)
	return nil
}

func (c *consoleConnectors) Approve(_ context.Context, connector, tool string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = append(c.set, connector+" "+tool+" approve")
	return nil
}

func TestATeamsLeadChoosesItsConnectorsInTheConsole(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].Runner.UseConnectors(&consoleConnectors{})
	bo := w.Browser("acme").SignedIn("bo")
	page := bo.Get("/teams/Sales")
	if !strings.Contains(page.Body, `name="connectors" value="linear"`) || !strings.Contains(page.Body, `name="connectors" value="notion"`) {
		t.Fatalf("Sales' page should offer the network's connectors:\n%s", page.Text())
	}

	if r := bo.Post("/teams/Sales", url.Values{"connectors": {"", "linear"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("choosing Linear: %d %s", r.Status, r.Text())
	}
	if team, _ := w.Networks["acme"].Store.Team(context.Background(), "Sales"); !slices.Equal(team.Connectors, []string{"linear"}) {
		t.Errorf("Sales' connectors = %v, want linear", team.Connectors)
	}
	if page := bo.Get("/teams/Sales"); !strings.Contains(page.Text(), "Linear") {
		t.Errorf("Sales' page should say it uses Linear:\n%s", page.Text())
	}
}

func TestConnectorToolsAreSetInTheConsole(t *testing.T) {
	w := NewWorld(t)
	c := &consoleConnectors{}
	w.Networks["acme"].Runner.UseConnectors(c)
	ada := w.Browser("acme").SignedIn("ada")

	page := ada.Get("/connectors")

	text := page.Text()
	for _, want := range []string{"Linear", "create_issue", "Create an issue, and email it", "Changed", "search_issues", "Notion", "Needs reconnecting",
		// A tool that runs others: its reads run, the rest ask.
		"execute_sentry_tool", "Reads run, changes ask first"} {
		if !strings.Contains(text, want) {
			t.Errorf("the connectors page should show %q:\n%s", want, text)
		}
	}
	if !strings.Contains(page.Body, `action="/connectors/linear/tools/create_issue"`) {
		t.Fatalf("the owner should be able to set create_issue:\n%s", page.Body)
	}
	if r := ada.Post("/connectors/linear/tools/create_issue", url.Values{"policy": {"allow"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("allowing create_issue: %d %s", r.Status, r.Text())
	}
	ada.Get("/connectors")
	if r := ada.Post("/connectors/linear/tools/create_issue", url.Values{"approve": {"1"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("approving create_issue: %d %s", r.Status, r.Text())
	}
	c.mu.Lock()
	if got := strings.Join(c.set, "|"); got != "linear create_issue policy allow|linear create_issue approve" {
		t.Errorf("set %q", got)
	}
	c.mu.Unlock()

	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/connectors"); strings.Contains(page.Body, `action="/connectors/`) {
		t.Error("bo approves only in Sales, so he can't set what connector tools may do")
	}
	if r := bo.Post("/connectors/linear/tools/create_issue", url.Values{"policy": {"allow"}}); r.Status != http.StatusForbidden {
		t.Errorf("bo setting a tool: %d, want 403", r.Status)
	}
}
