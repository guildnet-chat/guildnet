// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Signing in on the home page with Google (OpenID Connect, the
// authorization code flow with PKCE). The ID token comes straight from
// Google's token endpoint over TLS, so, as OpenID Connect allows, that
// connection vouches for it and its signature isn't checked; its claims
// still are: who issued it, for which app, when, and for which sign-in.

// GoogleSignIn is the platform's Google OAuth client.
type GoogleSignIn struct {
	ClientID, ClientSecret string
	// Google's endpoints; "" means Google's own. Tests point these at a fake.
	AuthURL, TokenURL string
}

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleWindow   = 10 * time.Minute // how long a sign-in may take at Google
)

func (g GoogleSignIn) authURL() string {
	if g.AuthURL != "" {
		return g.AuthURL
	}
	return googleAuthURL
}

func (g GoogleSignIn) tokenURL() string {
	if g.TokenURL != "" {
		return g.TokenURL
	}
	return googleTokenURL
}

// scheme is how this platform is reached.
func (p *Platform) scheme() string {
	if p.opts.SecureCookies {
		return "https"
	}
	return "http"
}

// homeURL is an address on the home page (the base domain), with the port
// the request came in on.
func (p *Platform) homeURL(r *http.Request, path string) string {
	return p.scheme() + "://" + r.Host + path
}

// googleCookie holds a sign-in in progress: its state, nonce and PKCE
// verifier, so only the browser that started it can finish it.
func (p *Platform) googleCookie() string { return p.cookieName() + "-google" }

func (p *Platform) googleStart(w http.ResponseWriter, r *http.Request) {
	g := p.opts.Google
	if g == nil {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier := newToken(), newToken(), newToken()
	http.SetCookie(w, &http.Cookie{Name: p.googleCookie(), Value: state + "." + nonce + "." + verifier, Path: "/",
		MaxAge: int(googleWindow.Seconds()), HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {p.homeURL(r, "/signin/google/callback")},
		"response_type":         {"code"},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	http.Redirect(w, r, g.authURL()+"?"+q.Encode(), http.StatusSeeOther)
}

func (p *Platform) googleCallback(w http.ResponseWriter, r *http.Request) {
	g := p.opts.Google
	if g == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(status int, msg string, err error) {
		if err != nil {
			monitor.Problem(p.opts.Logger, "google sign-in: %v", err)
		}
		p.renderHome(w, r, status, "home-signin", withError{Err: msg})
	}
	// The sign-in in progress is used up either way.
	http.SetCookie(w, &http.Cookie{Name: p.googleCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	c, err := r.Cookie(p.googleCookie())
	var parts []string
	if err == nil {
		parts = strings.Split(c.Value, ".")
	}
	state := r.URL.Query().Get("state")
	if err != nil || len(parts) != 3 || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(parts[0])) != 1 {
		fail(http.StatusBadRequest, "That sign-in didn't start here, or took too long. Try again.", nil)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail(http.StatusUnauthorized, "Google didn't sign you in. Try again.", nil)
		return
	}
	nonce, verifier := parts[1], parts[2]
	claims, err := p.exchangeGoogleCode(r, *g, r.URL.Query().Get("code"), verifier)
	if err != nil {
		fail(http.StatusBadGateway, "Couldn't finish signing in with Google. Try again.", err)
		return
	}
	if err := claims.check(g.ClientID, nonce, p.opts.Now()); err != nil {
		fail(http.StatusUnauthorized, "Couldn't sign you in with that Google account.", err)
		return
	}
	person, err := p.opts.People.GooglePerson(r.Context(), claims.Subject, claims.Email)
	if err != nil {
		fail(http.StatusInternalServerError, "Couldn't sign you in. Try again in a moment.", err)
		return
	}
	if person.Blocked {
		fail(http.StatusForbidden, p.blockedMessage(), nil)
		return
	}
	if err := p.signInHome(w, r, person, "/networks"); err != nil {
		fail(http.StatusInternalServerError, "Couldn't sign you in. Try again in a moment.", err)
	}
}

// idClaims are what GuildNet reads from a Google ID token.
type idClaims struct {
	Issuer        string          `json:"iss"`
	Audience      json.RawMessage `json:"aud"` // a string, or a list of them
	Subject       string          `json:"sub"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
	Nonce         string          `json:"nonce"`
	Expires       int64           `json:"exp"`
}

func (c idClaims) check(clientID, nonce string, now time.Time) error {
	if c.Issuer != "https://accounts.google.com" && c.Issuer != "accounts.google.com" {
		return fmt.Errorf("issued by %q", c.Issuer)
	}
	var one string
	var many []string
	if json.Unmarshal(c.Audience, &one) == nil {
		many = []string{one}
	} else if json.Unmarshal(c.Audience, &many) != nil {
		return errors.New("no audience")
	}
	if len(many) != 1 || many[0] != clientID {
		return fmt.Errorf("for %v, not this app", many)
	}
	if !now.Before(time.Unix(c.Expires, 0)) {
		return errors.New("expired")
	}
	if subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 {
		return errors.New("for another sign-in (nonce)")
	}
	if c.Subject == "" || c.Email == "" || !c.EmailVerified {
		return errors.New("no verified email")
	}
	return nil
}

// exchangeGoogleCode trades the code Google sent back for the ID token.
func (p *Platform) exchangeGoogleCode(r *http.Request, g GoogleSignIn, code, verifier string) (idClaims, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"redirect_uri":  {p.homeURL(r, "/signin/google/callback")},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", g.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return idClaims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return idClaims{}, err
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return idClaims{}, err
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return idClaims{}, fmt.Errorf("token endpoint: %s %s", resp.Status, out.Error)
	}
	parts := strings.Split(out.IDToken, ".")
	if len(parts) != 3 {
		return idClaims{}, errors.New("the ID token isn't a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idClaims{}, err
	}
	var c idClaims
	return c, json.Unmarshal(payload, &c)
}
