// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Signing in on the home page by email, beside Google. GuildNet emails a
// link and a 6-digit code. The link works once, within 15 minutes, and only
// in the browser that asked (a cookie says which), so a link sent to
// someone else can't sign them in as its sender, and a mail scanner that
// opens it uses nothing up. The code is for reading the email on another
// device: typed in the browser that asked, with five tries.

const (
	emailLinkTTL   = 15 * time.Minute
	emailCodeTries = 5
	linksAnAddress = 5  // sign-in emails an hour to one address
	linksAVisitor  = 20 // and from one visitor
)

type emailSignIn struct {
	email   string
	browser string // the hash of the asking browser's cookie
	code    string
	expires time.Time
	wrong   int
}

type emailSignIns struct {
	mu sync.Mutex
	m  map[string]*emailSignIn // by the hash of the link's secret
}

func (p *Platform) emailCookie() string { return p.cookieName() + "-email" }

func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// homeSigninView is the sign-in page's ways in.
func (p *Platform) homeSigninView() homeSignin {
	return homeSignin{Google: p.opts.Google != nil, Dev: p.opts.DevSignIn, Email: p.opts.Mail != nil && p.opts.People != nil}
}

func (p *Platform) askForSignInEmail(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	again := func(status int, msg string) {
		p.renderHome(w, r, status, "home-signin", withError{Err: msg, View: p.homeSigninView()})
	}
	if !mail.Plausible(email) {
		again(http.StatusBadRequest, "That doesn't look like an email address.")
		return
	}
	now, visitor := p.opts.Now(), p.visitor(r)
	if !p.linksByVisitor.Try(signInSource(visitor), now) || !p.linksByAddress.Try(email, now) {
		again(http.StatusTooManyRequests, "That's a lot of sign-in emails. Use the newest one you have, or wait an hour and ask again.")
		return
	}

	secret, browser, code := newToken(), newToken(), newCode()
	p.emailSignIns.mu.Lock()
	if p.emailSignIns.m == nil {
		p.emailSignIns.m = map[string]*emailSignIn{}
	}
	for k, x := range p.emailSignIns.m {
		if !now.Before(x.expires) {
			delete(p.emailSignIns.m, k)
		}
	}
	p.emailSignIns.m[hashToken(secret)] = &emailSignIn{email: email, browser: hashToken(browser), code: code, expires: now.Add(emailLinkTTL)}
	p.emailSignIns.mu.Unlock()

	if err := p.opts.Mail.Send(r.Context(), p.message(signInEmail(email, p.homeURL(r, "/signin/email/"+secret), code))); err != nil {
		monitor.Problem(p.opts.Logger, "emailing a sign-in link: %v", err)
		again(http.StatusBadGateway, "Couldn't send the email just now. Try again in a moment.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: p.emailCookie(), Value: browser, Path: "/", MaxAge: int(emailLinkTTL.Seconds()),
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	p.renderHome(w, r, http.StatusOK, "home-signin-email", withError{View: email})
}

// thisBrowser is the hash of the cookie saying which browser asked, or "".
func (p *Platform) thisBrowser(r *http.Request) string {
	c, err := r.Cookie(p.emailCookie())
	if err != nil || c.Value == "" {
		return ""
	}
	return hashToken(c.Value)
}

func (p *Platform) useSignInLink(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		p.renderHome(w, r, status, "home-signin", withError{Err: msg, View: p.homeSigninView()})
	}
	hash := hashToken(r.PathValue("token"))
	browser := p.thisBrowser(r)
	p.emailSignIns.mu.Lock()
	x := p.emailSignIns.m[hash]
	switch {
	case x == nil:
		p.emailSignIns.mu.Unlock()
		fail(http.StatusGone, "That link has been used, or has expired. Ask for a new one.")
		return
	case !p.opts.Now().Before(x.expires):
		delete(p.emailSignIns.m, hash)
		p.emailSignIns.mu.Unlock()
		fail(http.StatusGone, "That link has expired. Ask for a new one.")
		return
	case browser == "" || subtle.ConstantTimeCompare([]byte(browser), []byte(x.browser)) != 1:
		// Not used up: the browser that asked may still open it.
		p.emailSignIns.mu.Unlock()
		fail(http.StatusBadRequest, "This link works only in the browser where you asked for it. Open it there, or type the code from the email there.")
		return
	}
	delete(p.emailSignIns.m, hash)
	p.emailSignIns.mu.Unlock()
	p.finishEmailSignIn(w, r, x.email)
}

func (p *Platform) enterSignInCode(w http.ResponseWriter, r *http.Request) {
	browser := p.thisBrowser(r)
	code := normalCode(r.PostFormValue("code"))
	p.emailSignIns.mu.Lock()
	var hash string
	var x *emailSignIn
	for k, v := range p.emailSignIns.m {
		if browser != "" && v.browser == browser && p.opts.Now().Before(v.expires) {
			hash, x = k, v
		}
	}
	if x == nil {
		p.emailSignIns.mu.Unlock()
		p.renderHome(w, r, http.StatusBadRequest, "home-signin", withError{Err: "That sign-in has expired. Ask for a new link.", View: p.homeSigninView()})
		return
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(x.code)) != 1 {
		x.wrong++
		if x.wrong >= emailCodeTries {
			delete(p.emailSignIns.m, hash)
			p.emailSignIns.mu.Unlock()
			p.renderHome(w, r, http.StatusUnauthorized, "home-signin", withError{Err: "Too many wrong codes. Ask for a new link.", View: p.homeSigninView()})
			return
		}
		email := x.email
		p.emailSignIns.mu.Unlock()
		p.renderHome(w, r, http.StatusUnauthorized, "home-signin-email", withError{Err: "That code didn't match. Check the newest email from " + p.brand().Name + ".", View: email})
		return
	}
	delete(p.emailSignIns.m, hash)
	p.emailSignIns.mu.Unlock()
	p.finishEmailSignIn(w, r, x.email)
}

// finishEmailSignIn signs in whoever has email, now that they've shown
// they do.
func (p *Platform) finishEmailSignIn(w http.ResponseWriter, r *http.Request, email string) {
	http.SetCookie(w, &http.Cookie{Name: p.emailCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	person, err := p.opts.People.EmailPerson(r.Context(), email)
	if err == nil && person.Blocked {
		p.renderHome(w, r, http.StatusForbidden, "home-signin", withError{Err: p.blockedMessage(), View: p.homeSigninView()})
		return
	}
	if err == nil {
		err = p.signInHome(w, r, person, "/networks")
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "email sign-in: %v", err)
		p.renderHome(w, r, http.StatusInternalServerError, "home-signin", withError{Err: "Couldn't sign you in. Try again in a moment.", View: p.homeSigninView()})
	}
}

// signInEmail carries a sign-in link, and the code to type instead.
func signInEmail(email, link, code string) Letter {
	return Letter{To: email, Subject: "Your " + ourName + " sign-in link",
		Heading: "Sign in to " + ourName,
		Paras:   []string{"Someone, hopefully you, asked to sign in to " + ourName + " as " + email + ". The link works once, within 15 minutes, in the browser where you asked."},
		Button:  "Sign in to " + ourName, Link: link,
		After: []string{"Reading this on another device? Type the code in the browser where you asked instead: your code is " + code + "."},
		Why:   "If you didn't ask for this, you can ignore it: nobody can sign in without this email.",
	}
}
