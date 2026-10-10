// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Connectors: services a network's agents use as tools (Linear, Notion,
// Sentry, Atlassian). Only the network's owner connects one, from the
// network's page, signed in with Google, as with GitHub: whatever agents do
// through it shows as the account the owner connects with. The tokens stay
// here, sealed; the network's runner gets short-lived access tokens from the
// broker, one call at a time.

// Connection is a network's connection to one connector.
type Connection struct {
	Connector string // its ID
	Server    connectors.Server
	Client    connectors.Client
	Tokens    connectors.Tokens
	// Reconnect says the service refused its refresh token: the owner has to
	// connect it again.
	Reconnect bool
	By        string // who connected it (a Person's ID)
	At        time.Time
}

// Connections keeps networks' connections, and GuildNet's registrations
// with services' authorization servers (one per server and callback, shared
// by every network).
type Connections interface {
	Connections(ctx context.Context, network string) ([]Connection, error)
	Connection(ctx context.Context, network, connector string) (Connection, error) // ErrNotFound if none
	SaveConnection(ctx context.Context, network string, c Connection) error
	RemoveConnection(ctx context.Context, network, connector string) error // ErrNotFound if none
	// UpdateConnection changes a connection, one change at a time, even
	// across platforms: change is given it as it is now. An error from change
	// leaves it as it was.
	UpdateConnection(ctx context.Context, network, connector string, change func(*Connection) error) (Connection, error)
	OAuthClient(ctx context.Context, issuer, redirect string) (connectors.Client, bool, error)
	SaveOAuthClient(ctx context.Context, issuer, redirect string, c connectors.Client) error
}

// connectWindow is how long saying yes at the service may take.
const connectWindow = 10 * time.Minute

// pendingConnect is a connect in progress, by its state.
type pendingConnect struct {
	network, connector, person string
	console                    bool // started in the network's console, by its owner
	verifier, redirect         string
	server                     connectors.Server
	client                     connectors.Client
	expires                    time.Time
}

type pendingConnects struct {
	mu sync.Mutex
	m  map[string]pendingConnect // by the state's hash
}

func (p *pendingConnects) add(state string, c pendingConnect) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]pendingConnect{}
	}
	for k, x := range p.m {
		if time.Now().After(x.expires) {
			delete(p.m, k)
		}
	}
	p.m[hashToken(state)] = c
}

// peek is the connect state started, left as it is.
func (p *pendingConnects) peek(state string, now time.Time) (pendingConnect, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.m[hashToken(state)]
	return c, ok && now.Before(c.expires)
}

// take is the connect state started, used up.
func (p *pendingConnects) take(state string, now time.Time) (pendingConnect, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.m[hashToken(state)]
	delete(p.m, hashToken(state))
	return c, ok && now.Before(c.expires)
}

func (p *Platform) catalogue() []connectors.Connector {
	if p.opts.Connectors != nil {
		return p.opts.Connectors
	}
	return connectors.Catalogue
}

// oauth is GuildNet's side of connecting, coming back to this address.
func (p *Platform) oauth(redirect string) connectors.OAuth {
	return connectors.OAuth{HTTP: p.connectorHTTP, RedirectURL: redirect, MetadataURL: p.opts.ClientMetadataURL, Name: p.brand().Name, Now: p.opts.Now}
}

func (p *Platform) connectCookie() string {
	if p.opts.SecureCookies {
		return "__Host-guildnet-connect"
	}
	return "guildnet-connect"
}

// connectorView is a connector as the network's page shows it.
type connectorView struct {
	connectors.Connector
	Connected, Reconnect bool
	At                   time.Time
}

func (p *Platform) connectorViews(ctx context.Context, network string) []connectorView {
	if p.opts.Connections == nil {
		return nil
	}
	have, err := p.opts.Connections.Connections(ctx, network)
	if err != nil {
		monitor.Problem(p.opts.Logger, "%s's connections: %v", network, err)
	}
	var out []connectorView
	for _, c := range p.catalogue() {
		v := connectorView{Connector: c}
		for _, h := range have {
			if strings.EqualFold(h.Connector, c.ID) {
				v.Connected, v.Reconnect, v.At = true, h.Reconnect, h.At
			}
		}
		out = append(out, v)
	}
	return out
}

// connectService starts connecting a service to the owner's network: it
// finds the service's authorization server, registers with it if GuildNet
// hasn't, and sends the owner there. The last step is a page that moves on
// by itself: browsers hold a form's redirects to the page's form-action,
// and services' sign-in pages are anywhere.
func (p *Platform) connectService(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok || p.opts.Connections == nil {
		if ok {
			http.NotFound(w, r)
		}
		return
	}
	c, ok := connectors.Find(p.catalogue(), r.PathValue("connector"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	authorize, err := p.startConnect(w, r, n, c, homeOf(r).person.ID, false)
	if err != nil {
		p.networkPageWith(w, r, n, http.StatusBadGateway, "Couldn't reach "+c.Title+" to connect it. Try again in a moment.")
		return
	}
	p.renderHome(w, r, http.StatusOK, "home-onward", onward{Title: c.Title, Network: n.Name, URL: authorize})
}

// startConnect starts connecting c to n, for person (from the home page)
// or, in n's console, its owner: where they're to say yes, with a state
// only this browser will bring back.
func (p *Platform) startConnect(w http.ResponseWriter, r *http.Request, n Network, c connectors.Connector, person string, console bool) (string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Every connect comes back to the one address services know: the home
	// page's, which passes a console's on.
	redirect := p.homeOrigin(r) + "/oauth/callback"
	o := p.oauth(redirect)
	server, err := o.Discover(ctx, c)
	if err == nil {
		var client connectors.Client
		if client, err = p.oauthClient(ctx, o, server); err == nil {
			state := newToken()
			verifier, challenge := connectors.NewVerifier()
			p.connecting.add(state, pendingConnect{network: n.Name, connector: c.ID, person: person, console: console, verifier: verifier,
				redirect: redirect, server: server, client: client, expires: p.opts.Now().Add(connectWindow)})
			http.SetCookie(w, &http.Cookie{Name: p.connectCookie(), Value: state, Path: "/", MaxAge: int(connectWindow.Seconds()),
				HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
			return o.AuthorizeURL(server, client, state, challenge), nil
		}
	}
	monitor.Problem(p.opts.Logger, "connecting %s to %s: %v", n.Name, c.ID, err)
	return "", err
}

// onward is the page that moves on to a service's sign-in.
type onward struct{ Title, Network, URL string }

// oauthClient is GuildNet as a client of server's authorization server:
// registered once, then kept.
func (p *Platform) oauthClient(ctx context.Context, o connectors.OAuth, server connectors.Server) (connectors.Client, error) {
	if o.MetadataURL != "" && server.MetadataDocs {
		return o.Register(ctx, server)
	}
	if c, ok, err := p.opts.Connections.OAuthClient(ctx, server.Issuer, o.RedirectURL); err != nil || ok {
		return c, err
	}
	c, err := o.Register(ctx, server)
	if err != nil {
		return c, err
	}
	return c, p.opts.Connections.SaveOAuthClient(ctx, server.Issuer, o.RedirectURL, c)
}

// oauthCallback is the owner coming back from a service's sign-in. One
// that started in a network's console goes on there.
func (p *Platform) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if pc, ok := p.connecting.peek(q.Get("state"), p.opts.Now()); ok && pc.console {
		http.Redirect(w, r, p.networkOrigin(r, pc.network)+"/oauth/callback?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}
	// The connect in progress is used up either way.
	http.SetCookie(w, &http.Cookie{Name: p.connectCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	s, signedIn := p.loadHomeSession(r)
	cookie, err := r.Cookie(p.connectCookie())
	if !signedIn || err != nil || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(q.Get("state"))) != 1 {
		p.renderHome(w, r, http.StatusBadRequest, "home-signin", withError{Err: "That didn't start here, or took too long. Start connecting again from your network's page."})
		return
	}
	pc, ok := p.connecting.take(q.Get("state"), p.opts.Now())
	n, err := p.opts.Networks.Network(r.Context(), pc.network)
	if !ok || pc.person != s.person.ID || err != nil || n.Owner != s.person.ID {
		p.renderHome(w, r, http.StatusBadRequest, "home-signin", withError{Err: "That didn't start here, or took too long. Start connecting again from your network's page."})
		return
	}
	title, status, problem := p.finishConnect(r.Context(), n, pc, q, s.person.ID)
	if problem != "" {
		p.networkPageWith(w, r, n, status, problem)
		return
	}
	p.tellOwner(r.Context(), n, connectorEmail(n, p.homeURL(r, "/networks/"+n.Name), "connected", title, p.onHome(s.person.Email), p.opts.Now()))
	http.Redirect(w, r, "/networks/"+url.PathEscape(n.Name), http.StatusSeeOther)
}

// finishConnect is a connect coming back with the service's answer q,
// checked as started here: the connection's saved, as by by. If it isn't,
// what's said is why, with its status.
func (p *Platform) finishConnect(ctx context.Context, n Network, pc pendingConnect, q url.Values, by string) (title string, status int, problem string) {
	c, _ := connectors.Find(p.catalogue(), pc.connector)
	if q.Get("error") != "" {
		return c.Title, http.StatusOK, c.Title + " wasn't connected: " + c.Title + " said " + oneLine(q.Get("error"), 60) + "."
	}
	// RFC 9207: a server that names itself must name itself, so a code from
	// another server can't be passed off as this one's.
	if iss := q.Get("iss"); (iss != "" || pc.server.SaysIssuer) && iss != pc.server.Issuer {
		p.opts.Logger.Printf("connecting %s to %s: came back from %q, not %q", n.Name, c.ID, iss, pc.server.Issuer)
		return c.Title, http.StatusBadRequest, c.Title + " wasn't connected: the answer didn't come from " + c.Title + ". Try again."
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tokens, err := p.oauth(pc.redirect).Exchange(ctx, pc.server, pc.client, q.Get("code"), pc.verifier)
	if err != nil {
		monitor.Problem(p.opts.Logger, "connecting %s to %s: %v", n.Name, c.ID, err)
		return c.Title, http.StatusBadGateway, c.Title + " wasn't connected: it didn't give " + p.brand().Name + " access. Try again."
	}
	err = p.opts.Connections.SaveConnection(ctx, n.Name, Connection{Connector: c.ID, Server: pc.server, Client: pc.client, Tokens: tokens, By: by, At: p.opts.Now()})
	if err != nil {
		monitor.Problem(p.opts.Logger, "connecting %s to %s: saving: %v", n.Name, c.ID, err)
		return c.Title, http.StatusInternalServerError, "Couldn't save the connection. Try again."
	}
	p.opts.Logger.Printf("%s connected to %s", n.Name, c.ID)
	return c.Title, http.StatusOK, ""
}

// disconnectService forgets a network's connection, and asks the service to
// forget its tokens too.
func (p *Platform) disconnectService(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok || p.opts.Connections == nil {
		if ok {
			http.NotFound(w, r)
		}
		return
	}
	title, err := p.forgetConnection(r.Context(), n, r.PathValue("connector"))
	if errors.Is(err, ErrNotFound) {
		http.Redirect(w, r, "/networks/"+url.PathEscape(n.Name), http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(w, "Something went wrong. Try again in a moment.", http.StatusInternalServerError)
		return
	}
	p.tellOwner(r.Context(), n, connectorEmail(n, p.homeURL(r, "/networks/"+n.Name), "disconnected", title, p.onHome(homeOf(r).person.Email), p.opts.Now()))
	http.Redirect(w, r, "/networks/"+url.PathEscape(n.Name), http.StatusSeeOther)
}

// forgetConnection removes n's connection to connector, and asks the
// service to forget its tokens too: the connector's title, or ErrNotFound.
func (p *Platform) forgetConnection(ctx context.Context, n Network, connector string) (string, error) {
	c, err := p.opts.Connections.Connection(ctx, n.Name, connector)
	if err == nil {
		err = p.opts.Connections.RemoveConnection(ctx, n.Name, c.Connector)
	}
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			monitor.Problem(p.opts.Logger, "disconnecting %s from %s: %v", n.Name, connector, err)
		}
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, hint := c.Tokens.Refresh, "refresh_token"
	if token == "" {
		token, hint = c.Tokens.Access, "access_token"
	}
	if err := p.oauth("").Revoke(ctx, c.Server, c.Client, token, hint); err != nil {
		monitor.Problem(p.opts.Logger, "disconnecting %s from %s: revoking: %v", n.Name, c.Connector, err)
	}
	title := c.Connector
	if k, ok := connectors.Find(p.catalogue(), c.Connector); ok {
		title = k.Title
	}
	return title, nil
}

// The broker's part: runners' access tokens.

// refreshAhead is how long before a token ends it's refreshed.
const refreshAhead = 5 * time.Minute

// brokerConnectors lists the calling network's connections, for its runner.
func (p *Platform) brokerConnectors(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(r)
	if !ok {
		brokerError(w, http.StatusUnauthorized, "the network's secret is required")
		return
	}
	list := []map[string]any{}
	if p.opts.Connections != nil {
		have, err := p.opts.Connections.Connections(r.Context(), n.Name)
		if err != nil {
			monitor.Problem(p.opts.Logger, "%s's connections, for its runner: %v", n.Name, err)
			brokerError(w, http.StatusInternalServerError, "couldn't read the network's connections")
			return
		}
		for _, c := range have {
			k, ok := connectors.Find(p.catalogue(), c.Connector)
			if !ok {
				continue
			}
			list = append(list, map[string]any{"id": k.ID, "title": k.Title, "url": k.URL, "reconnect": c.Reconnect})
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"connectors": list})
}

// brokerConnectorToken gives the calling network's runner an access token
// for one of its connections, refreshed first if it's near its end. The
// refresh token never leaves the platform.
func (p *Platform) brokerConnectorToken(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(r)
	if !ok {
		brokerError(w, http.StatusUnauthorized, "the network's secret is required")
		return
	}
	if p.opts.Connections == nil {
		brokerError(w, http.StatusNotFound, "this network isn't connected to that")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	refused := false
	c, err := p.opts.Connections.UpdateConnection(ctx, n.Name, r.PathValue("connector"), func(c *Connection) error {
		if c.Reconnect || c.Tokens.Expires.IsZero() || p.opts.Now().Add(refreshAhead).Before(c.Tokens.Expires) {
			return nil
		}
		if c.Tokens.Refresh == "" {
			c.Reconnect, refused = true, true
			return nil
		}
		t, err := p.oauth("").Refresh(ctx, c.Server, c.Client, c.Tokens.Refresh)
		if errors.Is(err, connectors.ErrRefused) {
			c.Reconnect, refused = true, true
			return nil
		}
		if err != nil {
			return err
		}
		c.Tokens = t
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		brokerError(w, http.StatusNotFound, "this network isn't connected to that")
		return
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "refreshing %s's %s token: %v", n.Name, r.PathValue("connector"), err)
		brokerError(w, http.StatusBadGateway, "couldn't refresh the token; try again in a moment")
		return
	}
	title := c.Connector
	if k, ok := connectors.Find(p.catalogue(), c.Connector); ok {
		title = k.Title
	}
	if refused {
		p.opts.Logger.Printf("%s's %s connection was refused: it needs reconnecting", n.Name, c.Connector)
		p.tellOwner(r.Context(), n, reconnectEmail(n, p.scheme()+"://"+p.opts.BaseDomain, title))
	}
	if c.Reconnect {
		brokerError(w, http.StatusConflict, title+" needs reconnecting: the network's owner connects it again on the console's Connectors page")
		return
	}
	out := map[string]any{"token": c.Tokens.Access}
	if !c.Tokens.Expires.IsZero() {
		out["expires_at"] = c.Tokens.Expires
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// reconnectEmail tells the owner a service refused GuildNet's access: to
// connect it again from the network's page on the home page (at home), or,
// with nobody owning it there, its console's Connectors page.
func reconnectEmail(n Network, home, service string) Letter {
	again, button, page := "Connect it again from "+n.Name+"'s page on "+ourName+".", "Open "+n.Name+"'s page", home+"/networks/"+n.Name
	if n.Owner == "" {
		console := strings.Replace(home, "://", "://"+n.Name+".", 1)
		again, button, page = "Connect it again on "+n.Name+"'s Connectors page.", "Open Connectors", console+"/connectors"
	}
	return Letter{Subject: service + " needs reconnecting on " + n.Name,
		Heading: service + " needs reconnecting",
		Paras: []string{service + " stopped accepting " + n.Name + "'s connection (someone may have taken " + ourName + "'s access away there), so its agents can't use " + service + " for now.",
			again},
		Button: button, Link: page,
		Why: fmt.Sprintf(ownerWhy, n.Name),
	}
}

// networkPageWith is the network's page, saying msg.
func (p *Platform) networkPageWith(w http.ResponseWriter, r *http.Request, n Network, status int, msg string) {
	p.letFormsReach(w, r, n)
	p.renderHome(w, r, status, "home-network", withError{Err: msg, View: p.view(r, n)})
}

// clientMetadata is GuildNet's client ID metadata document: what services
// that take them know GuildNet by, its client ID being this document's
// address.
func (p *Platform) clientMetadata(w http.ResponseWriter, r *http.Request) {
	if p.opts.ClientMetadataURL == "" {
		http.NotFound(w, r)
		return
	}
	home := p.scheme() + "://" + r.Host
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	json.NewEncoder(w).Encode(map[string]any{
		"client_id": p.opts.ClientMetadataURL, "client_name": p.brand().Name, "client_uri": home + "/", "logo_uri": home + "/apple-touch-icon.png",
		"redirect_uris": []string{home + "/oauth/callback"}, "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": "none",
	})
}

// connectorEmail alerts the owner that a service was connected to the
// network, or disconnected.
func connectorEmail(n Network, page, what, service, where string, at time.Time) Letter {
	to := "to"
	if what == "disconnected" {
		to = "from"
	}
	return Letter{Subject: n.Name + " was " + what + " " + to + " " + service,
		Heading: n.Name + " was " + what + " " + to + " " + service,
		Paras: []string{n.Name + " was " + what + " " + to + " " + service + " from " + where + ", at " + stamp(at) + ".",
			notYou},
		Button: "Open " + n.Name + "'s page", Link: page,
		Why: securityWhy,
	}
}

// newConnectorHTTP is how the platform reaches services: public HTTPS
// addresses only.
func newConnectorHTTP(o egress.Options) *http.Client { return egress.Client(o) }

// Memory.

func connKey(a, b string) string { return strings.ToLower(a) + "|" + strings.ToLower(b) }

func (s *MemoryStore) Connections(_ context.Context, network string) ([]Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Connection
	for k, c := range s.connections {
		if strings.HasPrefix(k, strings.ToLower(network)+"|") {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *MemoryStore) Connection(_ context.Context, network, connector string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connections[connKey(network, connector)]
	if !ok {
		return Connection{}, ErrNotFound
	}
	return c, nil
}

func (s *MemoryStore) SaveConnection(_ context.Context, network string, c Connection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connections == nil {
		s.connections = map[string]Connection{}
	}
	s.connections[connKey(network, c.Connector)] = c
	return nil
}

func (s *MemoryStore) RemoveConnection(_ context.Context, network, connector string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.connections[connKey(network, connector)]; !ok {
		return ErrNotFound
	}
	delete(s.connections, connKey(network, connector))
	return nil
}

func (s *MemoryStore) UpdateConnection(ctx context.Context, network, connector string, change func(*Connection) error) (Connection, error) {
	// One change at a time; mu isn't held while change runs (it may call the
	// service), so everything else carries on.
	s.connMu.Lock()
	defer s.connMu.Unlock()
	c, err := s.Connection(ctx, network, connector)
	if err != nil {
		return c, err
	}
	if err := change(&c); err != nil {
		return c, err
	}
	return c, s.SaveConnection(ctx, network, c)
}

func (s *MemoryStore) OAuthClient(_ context.Context, issuer, redirect string) (connectors.Client, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.oauthClients[issuer+"|"+redirect]
	return c, ok, nil
}

func (s *MemoryStore) SaveOAuthClient(_ context.Context, issuer, redirect string, c connectors.Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oauthClients == nil {
		s.oauthClients = map[string]connectors.Client{}
	}
	s.oauthClients[issuer+"|"+redirect] = c
	return nil
}

// dropConnections forgets a removed network's connections. Callers hold mu.
func (s *MemoryStore) dropConnections(network string) {
	for k := range s.connections {
		if strings.HasPrefix(k, strings.ToLower(network)+"|") {
			delete(s.connections, k)
		}
	}
}

// Postgres. Tokens and client secrets are sealed.

// sealString seals s, bound to aad; "" stays nil.
func (s *Postgres) sealString(plain string, aad []byte) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return s.sealer.Seal([]byte(plain), aad)
}

func (s *Postgres) openString(sealed, aad []byte) (string, error) {
	if sealed == nil {
		return "", nil
	}
	b, err := s.sealer.Open(sealed, aad)
	return string(b), err
}

// connectionSecrets are where a connection's secrets are kept, and so
// bound: the network and the service, and the server and client they're
// sent to (so changing where the token endpoint is, in the database,
// doesn't send them elsewhere).
func connectionSecrets(network, connector string, server connectors.Server, client connectors.Client) (secret, access, refresh []byte, err error) {
	srv, err := json.Marshal(server)
	if err != nil {
		return nil, nil, nil, err
	}
	network, connector = strings.ToLower(network), strings.ToLower(connector)
	return aad("connections", "client_secret", network, connector, string(srv), client.ID, client.Method),
		aad("connections", "access_token", network, connector, string(srv), client.ID),
		aad("connections", "refresh_token", network, connector, string(srv), client.ID, client.Method), nil
}

// oauthClientSecret is where a service's client secret for GuildNet is kept.
func oauthClientSecret(issuer, redirect string, c connectors.Client) []byte {
	return aad("oauth_clients", "client_secret", issuer, redirect, c.ID, c.Method)
}

const connectionColumns = `connector, server, client_id, client_secret, auth_method, access_token, refresh_token, expires_at, scope, reconnect, connected_by, connected_at`

func (s *Postgres) scanConnection(network string, row pgx.Row) (Connection, error) {
	var c Connection
	var server []byte
	var secret, access, refresh []byte
	var expires *time.Time
	if err := row.Scan(&c.Connector, &server, &c.Client.ID, &secret, &c.Client.Method, &access, &refresh, &expires, &c.Tokens.Scope, &c.Reconnect, &c.By, &c.At); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNotFound
		}
		return c, err
	}
	if err := json.Unmarshal(server, &c.Server); err != nil {
		return c, err
	}
	if expires != nil {
		c.Tokens.Expires = *expires
	}
	secretAAD, accessAAD, refreshAAD, err := connectionSecrets(network, c.Connector, c.Server, c.Client)
	if err != nil {
		return c, err
	}
	for _, f := range []struct {
		sealed, aad []byte
		into        *string
	}{{secret, secretAAD, &c.Client.Secret}, {access, accessAAD, &c.Tokens.Access}, {refresh, refreshAAD, &c.Tokens.Refresh}} {
		if *f.into, err = s.openString(f.sealed, f.aad); err != nil {
			return c, fmt.Errorf("%s's %s connection: %w", network, c.Connector, err)
		}
	}
	return c, nil
}

func (s *Postgres) Connections(ctx context.Context, network string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, `select `+connectionColumns+` from connections where network = $1 order by connector`, strings.ToLower(network))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := s.scanConnection(network, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Postgres) Connection(ctx context.Context, network, connector string) (Connection, error) {
	return s.scanConnection(network, s.pool.QueryRow(ctx, `select `+connectionColumns+` from connections where network = $1 and connector = $2`,
		strings.ToLower(network), strings.ToLower(connector)))
}

func (s *Postgres) SaveConnection(ctx context.Context, network string, c Connection) error {
	return s.saveConnection(ctx, s.pool, network, c)
}

// UpdateConnection holds the connection's row while change runs, so
// platforms (and requests) change it one at a time.
func (s *Postgres) UpdateConnection(ctx context.Context, network, connector string, change func(*Connection) error) (Connection, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	c, err := s.scanConnection(network, tx.QueryRow(ctx, `select `+connectionColumns+` from connections where network = $1 and connector = $2 for update`,
		strings.ToLower(network), strings.ToLower(connector)))
	if err != nil {
		return c, err
	}
	if err := change(&c); err != nil {
		return c, err
	}
	if err := s.saveConnection(ctx, tx, network, c); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Postgres) saveConnection(ctx context.Context, db execer, network string, c Connection) error {
	server, err := json.Marshal(c.Server)
	if err != nil {
		return err
	}
	secretAAD, accessAAD, refreshAAD, err := connectionSecrets(network, c.Connector, c.Server, c.Client)
	if err != nil {
		return err
	}
	var sealed [3][]byte
	for i, f := range []struct {
		plain string
		aad   []byte
	}{{c.Client.Secret, secretAAD}, {c.Tokens.Access, accessAAD}, {c.Tokens.Refresh, refreshAAD}} {
		if sealed[i], err = s.sealString(f.plain, f.aad); err != nil {
			return err
		}
	}
	var expires *time.Time
	if !c.Tokens.Expires.IsZero() {
		expires = &c.Tokens.Expires
	}
	_, err = db.Exec(ctx, `insert into connections (network, `+connectionColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		on conflict (network, connector) do update set server = $3, client_id = $4, client_secret = $5, auth_method = $6,
			access_token = $7, refresh_token = $8, expires_at = $9, scope = $10, reconnect = $11, connected_by = $12, connected_at = $13`,
		strings.ToLower(network), strings.ToLower(c.Connector), server, c.Client.ID, sealed[0], c.Client.Method, sealed[1], sealed[2],
		expires, c.Tokens.Scope, c.Reconnect, c.By, c.At)
	return err
}

func (s *Postgres) RemoveConnection(ctx context.Context, network, connector string) error {
	tag, err := s.pool.Exec(ctx, `delete from connections where network = $1 and connector = $2`, strings.ToLower(network), strings.ToLower(connector))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) OAuthClient(ctx context.Context, issuer, redirect string) (connectors.Client, bool, error) {
	var c connectors.Client
	var secret []byte
	err := s.pool.QueryRow(ctx, `select client_id, client_secret, auth_method from oauth_clients where issuer = $1 and redirect_uri = $2`, issuer, redirect).
		Scan(&c.ID, &secret, &c.Method)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	c.Secret, err = s.openString(secret, oauthClientSecret(issuer, redirect, c))
	return c, err == nil, err
}

func (s *Postgres) SaveOAuthClient(ctx context.Context, issuer, redirect string, c connectors.Client) error {
	secret, err := s.sealString(c.Secret, oauthClientSecret(issuer, redirect, c))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into oauth_clients (issuer, redirect_uri, client_id, client_secret, auth_method) values ($1, $2, $3, $4, $5)
		on conflict (issuer, redirect_uri) do update set client_id = $3, client_secret = $4, auth_method = $5`, issuer, redirect, c.ID, secret, c.Method)
	return err
}
