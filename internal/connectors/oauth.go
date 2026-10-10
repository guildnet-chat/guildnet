// SPDX-License-Identifier: AGPL-3.0-only

package connectors

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Connecting follows the MCP authorization spec: GuildNet asks the MCP
// server where its authorization server is (its protected resource
// metadata), checks that server's own metadata names the same issuer,
// registers with it (by pointing it at GuildNet's client ID metadata
// document, or with dynamic registration), and sends the owner there with
// PKCE (S256) and the server as the token's resource.

// Server is where and how a connector's MCP server is authorized, as found
// when connecting it.
type Server struct {
	Resource  string `json:"resource"` // the MCP server, as its tokens name it
	Issuer    string `json:"issuer"`
	Authorize string `json:"authorize"`
	Token     string `json:"token"`
	Register  string `json:"register,omitempty"`
	Revoke    string `json:"revoke,omitempty"`
	// Scopes are what's asked for: what the server said it wants, or what it
	// offers.
	Scopes []string `json:"scopes,omitempty"`
	// MetadataDocs says it takes client ID metadata documents.
	MetadataDocs bool `json:"metadata_docs,omitempty"`
	// SaysIssuer says it names itself (iss) when sending people back, which
	// is then required.
	SaysIssuer  bool     `json:"says_issuer,omitempty"`
	AuthMethods []string `json:"auth_methods,omitempty"` // how clients may prove who they are
}

// Client is GuildNet as a client of one authorization server.
type Client struct {
	ID     string `json:"id"`
	Secret string `json:"-"` // kept sealed, apart
	// Method is how GuildNet proves it's the client when asking for tokens:
	// "none", "client_secret_basic" or "client_secret_post".
	Method string `json:"method"`
}

// Tokens are what connecting (or refreshing) gives.
type Tokens struct {
	Access  string
	Refresh string    // "" if none was given
	Expires time.Time // zero if the server didn't say
	Scope   string
}

// ErrRefused means the authorization server refused a refresh token (or a
// code): the owner has to connect again.
var ErrRefused = errors.New("the service refused GuildNet's sign-in")

// OAuth is GuildNet's side of connecting.
type OAuth struct {
	// HTTP reaches servers; only public HTTPS addresses (package egress).
	HTTP        *http.Client
	RedirectURL string // where people come back to, e.g. https://guildnet.chat/oauth/callback
	// MetadataURL is GuildNet's client ID metadata document, used as its
	// client ID where servers take one; "" means registering dynamically.
	MetadataURL string
	Name        string           // "GuildNet", as servers show it
	Now         func() time.Time // nil means time.Now
}

func (o OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Discover finds how c's server is authorized.
func (o OAuth) Discover(ctx context.Context, c Connector) (Server, error) {
	hint, scope := o.challenge(ctx, c.URL)
	s := Server{Resource: c.URL}
	var issuer string
	for _, u := range resourceMetadataURLs(hint, c.URL) {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, u.metadata, u.resource, o.HTTP)
		if err != nil || len(prm.AuthorizationServers) == 0 {
			continue
		}
		s.Resource, issuer = prm.Resource, prm.AuthorizationServers[0]
		if scope == "" {
			s.Scopes = prm.ScopesSupported
		}
		break
	}
	if issuer == "" {
		// An older server: its own issuer, or the catalogue's word for it.
		issuer = c.Issuer
		if issuer == "" {
			u, err := url.Parse(c.URL)
			if err != nil {
				return Server{}, err
			}
			issuer = u.Scheme + "://" + u.Host
		}
	}
	if scope != "" {
		s.Scopes = strings.Fields(scope)
	}
	meta, err := auth.GetAuthServerMetadata(ctx, issuer, o.HTTP)
	if err != nil {
		return Server{}, fmt.Errorf("reading %s's sign-in details: %w", issuer, err)
	}
	if meta == nil {
		return Server{}, fmt.Errorf("%s doesn't say how to sign in", issuer)
	}
	if !slices.Contains(meta.CodeChallengeMethodsSupported, "S256") {
		return Server{}, fmt.Errorf("%s doesn't offer PKCE with S256", issuer)
	}
	s.Issuer, s.Authorize, s.Token, s.Register, s.Revoke = meta.Issuer, meta.AuthorizationEndpoint, meta.TokenEndpoint, meta.RegistrationEndpoint, meta.RevocationEndpoint
	s.MetadataDocs, s.SaysIssuer, s.AuthMethods = meta.ClientIDMetadataDocumentSupported, meta.AuthorizationResponseIssParameterSupported, meta.TokenEndpointAuthMethodsSupported
	if s.Authorize == "" || s.Token == "" {
		return Server{}, fmt.Errorf("%s doesn't say where to sign in", issuer)
	}
	return s, nil
}

// challenge is what the server says when called without a token: where its
// resource metadata is, and what scope it wants, if it says.
func (o OAuth) challenge(ctx context.Context, server string) (metadata, scope string) {
	req, err := http.NewRequestWithContext(ctx, "POST", server, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		return "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", ""
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return "", ""
	}
	cs, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return "", ""
	}
	for _, c := range cs {
		if c.Scheme == "bearer" {
			return c.Params["resource_metadata"], c.Params["scope"]
		}
	}
	return "", ""
}

type metadataURL struct{ metadata, resource string }

// resourceMetadataURLs are where a server's resource metadata may be: where
// it said, then the well-known addresses for its path and its host.
func resourceMetadataURLs(hint, server string) []metadataURL {
	var out []metadataURL
	if hint != "" {
		out = append(out, metadataURL{hint, server})
	}
	u, err := url.Parse(server)
	if err != nil {
		return out
	}
	origin := u.Scheme + "://" + u.Host
	if p := strings.Trim(u.Path, "/"); p != "" {
		out = append(out, metadataURL{origin + "/.well-known/oauth-protected-resource/" + p, server})
	}
	return append(out, metadataURL{origin + "/.well-known/oauth-protected-resource", origin})
}

// Register makes GuildNet a client of s: by its client ID metadata document
// if s takes those, else by registering.
func (o OAuth) Register(ctx context.Context, s Server) (Client, error) {
	if o.MetadataURL != "" && s.MetadataDocs {
		return Client{ID: o.MetadataURL, Method: "none"}, nil
	}
	if s.Register == "" {
		return Client{}, fmt.Errorf("%s doesn't let GuildNet register with it", s.Issuer)
	}
	// A secret where the server gives one: then a refresh token alone isn't
	// enough to get tokens.
	method := "none"
	switch {
	case len(s.AuthMethods) == 0 || slices.Contains(s.AuthMethods, "client_secret_basic"):
		method = "client_secret_basic"
	case slices.Contains(s.AuthMethods, "client_secret_post"):
		method = "client_secret_post"
	}
	reg, err := oauthex.RegisterClient(ctx, s.Register, &oauthex.ClientRegistrationMetadata{
		RedirectURIs: []string{o.RedirectURL}, TokenEndpointAuthMethod: method, ClientName: o.Name,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		ApplicationType: "web", Scope: strings.Join(s.Scopes, " "),
	}, o.HTTP)
	if err != nil {
		return Client{}, fmt.Errorf("registering with %s: %w", s.Issuer, err)
	}
	c := Client{ID: reg.ClientID, Secret: reg.ClientSecret, Method: method}
	if c.Secret == "" {
		c.Method = "none"
	} else if m := reg.TokenEndpointAuthMethod; m == "client_secret_basic" || m == "client_secret_post" {
		c.Method = m
	}
	return c, nil
}

// NewVerifier is a PKCE verifier and its S256 challenge.
func NewVerifier() (verifier, challenge string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL is where the owner is sent to say yes.
func (o OAuth) AuthorizeURL(s Server, c Client, state, challenge string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {c.ID}, "redirect_uri": {o.RedirectURL},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {s.Resource}}
	if len(s.Scopes) > 0 {
		q.Set("scope", strings.Join(s.Scopes, " "))
	}
	sep := "?"
	if strings.Contains(s.Authorize, "?") {
		sep = "&"
	}
	return s.Authorize + sep + q.Encode()
}

// Exchange trades the code the owner came back with for tokens.
func (o OAuth) Exchange(ctx context.Context, s Server, c Client, code, verifier string) (Tokens, error) {
	return o.token(ctx, s, c, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {o.RedirectURL}, "code_verifier": {verifier}})
}

// Refresh trades a refresh token for new tokens. A server that doesn't
// give a new refresh token keeps the old one working.
func (o OAuth) Refresh(ctx context.Context, s Server, c Client, refresh string) (Tokens, error) {
	t, err := o.token(ctx, s, c, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err == nil && t.Refresh == "" {
		t.Refresh = refresh
	}
	return t, err
}

// Revoke asks the server to forget a token, where it offers that.
func (o OAuth) Revoke(ctx context.Context, s Server, c Client, token, hint string) error {
	if s.Revoke == "" || token == "" {
		return nil
	}
	resp, err := o.post(ctx, s.Revoke, c, url.Values{"token": {token}, "token_type_hint": {hint}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking at %s: %s", s.Issuer, resp.Status)
	}
	return nil
}

func (o OAuth) post(ctx context.Context, endpoint string, c Client, form url.Values) (*http.Response, error) {
	if c.Method != "client_secret_basic" {
		form.Set("client_id", c.ID)
	}
	if c.Method == "client_secret_post" {
		form.Set("client_secret", c.Secret)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.Method == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(c.ID), url.QueryEscape(c.Secret))
	}
	return o.HTTP.Do(req)
}

func (o OAuth) token(ctx context.Context, s Server, c Client, form url.Values) (Tokens, error) {
	form.Set("resource", s.Resource)
	resp, err := o.post(ctx, s.Token, c, form)
	if err != nil {
		return Tokens{}, fmt.Errorf("asking %s for tokens: %w", s.Issuer, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	json.NewDecoder(bytes.NewReader(body)).Decode(&out)
	switch {
	case out.Error == "invalid_grant":
		return Tokens{}, ErrRefused
	case resp.StatusCode != http.StatusOK || out.Error != "":
		return Tokens{}, fmt.Errorf("%s gave no tokens: %s %s", s.Issuer, resp.Status, out.Error)
	case out.AccessToken == "":
		return Tokens{}, fmt.Errorf("%s gave no access token", s.Issuer)
	case out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer"):
		return Tokens{}, fmt.Errorf("%s gave a %s token, not a bearer token", s.Issuer, out.TokenType)
	}
	t := Tokens{Access: out.AccessToken, Refresh: out.RefreshToken, Scope: out.Scope}
	if out.ExpiresIn > 0 {
		t.Expires = o.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return t, nil
}
