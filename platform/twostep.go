// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"rsc.io/qr"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Two-step sign-in to a network's console: after the password (or "Open"
// from the home page, or a reset link), a code from an authenticator app,
// or one of ten one-use recovery codes. Anyone may turn it on. Where the
// platform requires it, the owner and anyone who may change things across
// the whole org must: until they have, they're signed in only as far as
// setting it up.

// TwoStepSetup is how someone signs in to a network's console in two steps.
// The zero value is off.
type TwoStepSetup struct {
	Secret   string   // the authenticator app's, in base32; "" if they use passkeys only
	LastStep int64    // the last 30-second step whose code was used
	Recovery []string // hashes of the recovery codes not yet used
	Since    time.Time
	Passkeys []Passkey // oldest first
}

// On says whether it's set up: with the app, a passkey, or both.
func (t TwoStepSetup) On() bool { return t.Secret != "" || len(t.Passkeys) > 0 }

// Passkey is a passkey added as a second step: a phone, a laptop or a
// security key, for the network's own address.
type Passkey struct {
	ID         string // the credential's, base64url
	Name       string // what they called it
	Credential []byte // webauthn.Credential, as JSON: its public key and sign count
	Added      time.Time
	Used       time.Time // zero until it's signed in
}

// TwoSteps keeps people's two-step sign-in, by network and account.
type TwoSteps interface {
	TwoStepOf(ctx context.Context, network, account string) (TwoStepSetup, error) // zero if off
	// SetTwoStep keeps all but the passkeys, which change one by one.
	SetTwoStep(ctx context.Context, network, account string, t TwoStepSetup) error
	AddPasskey(ctx context.Context, network, account string, k Passkey) error
	// UpdatePasskey keeps a passkey's credential and when it was used.
	UpdatePasskey(ctx context.Context, network, account string, k Passkey) error
	RemovePasskey(ctx context.Context, network, account, id string) error
	// UseStep marks the app's code for step used, if no later or equal one
	// was: false if one was (the code has been used).
	UseStep(ctx context.Context, network, account string, step int64) (bool, error)
	// UseRecovery uses the recovery code with hash, once: false if it
	// isn't one of theirs; left is how many are left.
	UseRecovery(ctx context.Context, network, account, hash string) (left int, ok bool, err error)
	// RemoveTwoStep turns it off: the app, the passkeys and the codes.
	RemoveTwoStep(ctx context.Context, network, account string) error
}

// The authenticator app's codes (RFC 6238): SHA-1, 30 seconds, 6 digits, from
// a 20-byte secret; one step either side is allowed for clocks that drift.

const totpPeriod = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

func totpAt(key []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000)
}

// totpStep is the step whose code code is, near now, or false.
func totpStep(secret, code string, now time.Time) (int64, bool) {
	key, err := b32.DecodeString(secret)
	if err != nil || len(key) < 16 || len(code) != 6 {
		return 0, false
	}
	step := now.Unix() / totpPeriod
	for _, s := range []int64{step - 1, step, step + 1} {
		if subtle.ConstantTimeCompare([]byte(totpAt(key, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// totpLink is what an authenticator app scans to add account on network.
func (p *Platform) totpLink(network, account, secret string) string {
	label := url.PathEscape(p.brand().Name + " " + network + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {p.brand().Name + " " + network}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// qrImage is text as a QR code, as a data: address for an <img>.
func qrImage(text string) string {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return ""
	}
	code.Scale = 6
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
}

// Recovery codes: ten, each ten base32 letters shown as two groups of five.

const recoveryCodes = 10

func newRecoveryCodes() (codes, hashes []string) {
	for range recoveryCodes {
		b := make([]byte, 7)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		c := strings.ToLower(b32.EncodeToString(b))[:10]
		codes = append(codes, c[:5]+"-"+c[5:])
		hashes = append(hashes, hashToken(c))
	}
	return codes, hashes
}

// normalCode is a typed code without spaces or dashes, in lower case.
func normalCode(code string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, code))
}

// secondStep checks a code for account's second step on n: the app's (once),
// or a recovery code (once; the owner hears).
func (p *Platform) secondStep(ctx context.Context, r *http.Request, n Network, account, code string) (bool, error) {
	t, err := p.opts.TwoSteps.TwoStepOf(ctx, n.Name, account)
	if err != nil || !t.On() {
		return false, err
	}
	code = normalCode(code)
	if len(code) == 6 {
		step, ok := totpStep(t.Secret, code, p.opts.Now())
		if !ok {
			return false, nil
		}
		return p.opts.TwoSteps.UseStep(ctx, n.Name, account, step)
	}
	if len(code) != 10 {
		return false, nil
	}
	left, ok, err := p.opts.TwoSteps.UseRecovery(ctx, n.Name, account, hashToken(code))
	if ok {
		p.opts.Logger.Printf("%s on %s used a recovery code; %d left", account, n.Name, left)
		p.tellOwner(ctx, n, recoveryEmail(n, p.networkOrigin(r, n.Name), account, left, p.opts.Now()))
	}
	return ok, err
}

// Between the steps: a sign-in waiting for its second step, held for a few
// minutes in this browser (by a cookie) and in memory.

const (
	stepTTL      = 5 * time.Minute
	stepAttempts = 5 // wrong codes before starting again
)

type pendingStep struct {
	network, account string
	expires          time.Time
	wrong            int
	ceremony         *webauthn.SessionData // a passkey asked for, not yet answered
}

type pendingSteps struct {
	mu sync.Mutex
	m  map[string]*pendingStep // by the cookie's hash
}

func (p *Platform) stepCookie() string { return p.cookieName() + "-step" }

// signInAs signs account in to n's console, or, if they use two-step sign-in,
// sends them on to the second step.
func (p *Platform) signInAs(w http.ResponseWriter, r *http.Request, n Network, account string) error {
	on := false
	if p.opts.TwoSteps != nil {
		t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, account)
		if err != nil {
			return err
		}
		on = t.On()
	}
	if !on {
		if err := p.startSession(w, r, n, account); err != nil {
			return err
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return nil
	}
	p.awaitStep(w, n, account)
	http.Redirect(w, r, "/signin/two-step", http.StatusSeeOther)
	return nil
}

// awaitStep holds account's sign-in to n (homeAccount for the home page)
// until its second step, in this browser.
func (p *Platform) awaitStep(w http.ResponseWriter, n Network, account string) {
	token := newToken()
	now := p.opts.Now()
	p.steps.mu.Lock()
	if p.steps.m == nil {
		p.steps.m = map[string]*pendingStep{}
	}
	for k, v := range p.steps.m {
		if !now.Before(v.expires) {
			delete(p.steps.m, k)
		}
	}
	p.steps.m[hashToken(token)] = &pendingStep{network: n.Name, account: account, expires: now.Add(stepTTL)}
	p.steps.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: p.stepCookie(), Value: token, Path: "/", MaxAge: int(stepTTL.Seconds()),
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
}

// pending is this browser's sign-in waiting for its second step on n.
func (p *Platform) pending(r *http.Request, n Network) (string, *pendingStep) {
	c, err := r.Cookie(p.stepCookie())
	if err != nil || c.Value == "" {
		return "", nil
	}
	hash := hashToken(c.Value)
	p.steps.mu.Lock()
	defer p.steps.mu.Unlock()
	x := p.steps.m[hash]
	if x == nil || !strings.EqualFold(x.network, n.Name) || !p.opts.Now().Before(x.expires) {
		return "", nil
	}
	return hash, x
}

func (p *Platform) forgetPending(w http.ResponseWriter, hash string) {
	p.steps.mu.Lock()
	delete(p.steps.m, hash)
	p.steps.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: p.stepCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
}

// signedInAlready sends someone with a console session to the console, and
// forgets any second step left over from signing in again (another tab, or
// opening the network from the home page): there's nothing to ask them.
func (p *Platform) signedInAlready(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := sessionOf(r); !ok {
		return false
	}
	if hash, x := p.pending(r, networkOf(r)); x != nil {
		p.forgetPending(w, hash)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return true
}

func (p *Platform) secondStepPage(w http.ResponseWriter, r *http.Request) {
	if p.signedInAlready(w, r) {
		return
	}
	if _, x := p.pending(r, networkOf(r)); x == nil {
		p.render(w, r, http.StatusBadRequest, "signin", withError{Err: "That sign-in has expired. Sign in again."})
		return
	}
	p.render(w, r, http.StatusOK, "signin-two-step", withError{View: p.hasPasskeys(r)})
}

// tryStep counts a try at x's second step, before it's checked, and says
// whether x has one left: so codes sent at once get no more tries than codes
// sent one by one. Each try is also counted by account (codeAttempts.Try).
func (p *Platform) tryStep(x *pendingStep) bool {
	p.steps.mu.Lock()
	defer p.steps.mu.Unlock()
	if x.wrong >= stepAttempts {
		return false
	}
	x.wrong++
	return true
}

func (p *Platform) secondStepDone(w http.ResponseWriter, r *http.Request) {
	if p.signedInAlready(w, r) {
		return
	}
	n := networkOf(r)
	hash, x := p.pending(r, n)
	if x == nil {
		p.render(w, r, http.StatusBadRequest, "signin", withError{Err: "That sign-in has expired. Sign in again."})
		return
	}
	key := strings.ToLower(n.Name + "|" + x.account)
	now := p.opts.Now()
	if !p.tryStep(x) {
		p.forgetPending(w, hash)
		p.render(w, r, http.StatusTooManyRequests, "signin", withError{Err: "Too many wrong codes. Sign in again.", View: x.account})
		return
	}
	if !p.codeAttempts.Try(key, now) {
		p.forgetPending(w, hash)
		p.render(w, r, http.StatusTooManyRequests, "signin", withError{Err: "Too many wrong codes. Wait 15 minutes, then sign in again.", View: x.account})
		return
	}
	ok, err := p.secondStep(r.Context(), r, n, x.account, r.PostFormValue("code"))
	if err != nil {
		monitor.Problem(p.opts.Logger, "checking %s's second step on %s: %v", x.account, n.Name, err)
		p.render(w, r, http.StatusInternalServerError, "signin-two-step", withError{Err: "Couldn't check it just now. Try again in a moment.", View: p.hasPasskeys(r)})
		return
	}
	if !ok {
		p.steps.mu.Lock()
		wrong := x.wrong
		p.steps.mu.Unlock()
		if wrong >= stepAttempts {
			p.forgetPending(w, hash)
			p.render(w, r, http.StatusUnauthorized, "signin", withError{Err: "Too many wrong codes. Sign in again.", View: x.account})
			return
		}
		p.render(w, r, http.StatusUnauthorized, "signin-two-step", withError{Err: "That code didn't match. Try the newest one your app shows.", View: p.hasPasskeys(r)})
		return
	}
	p.forgetPending(w, hash)
	p.codeAttempts.Reset(key)
	signedIn, err := p.newSession(w, r, n, x.account)
	if err != nil {
		monitor.Problem(p.opts.Logger, "saving a session: %v", err)
		p.render(w, r, http.StatusInternalServerError, "signin", withError{Err: "Couldn't sign you in. Try again in a moment."})
		return
	}
	p.stepped.mark(signedIn, p.opts.Now())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Changing how someone signs in needs their second step shown recently, in
// the same sign-in: not just anyone at an unlocked computer.

// freshStep is how long after the second step that is.
const freshStep = 15 * time.Minute

// stepped is when each sign-in last showed its second step, by its hash.
// Kept in memory: after a restart, signing in again does.
type stepped struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (s *stepped) mark(hash string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	for k, t := range s.at {
		if now.Sub(t) > freshStep {
			delete(s.at, k)
		}
	}
	s.at[hash] = now
}

func (s *stepped) recently(hash string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[hash]
	return ok && hash != "" && now.Sub(t) <= freshStep
}

// stepUpNote is what someone changing their sign-in too long after the
// second step is told.
const stepUpNote = "Sign in again to change how you sign in: it asks for your second step first, so nobody at an unlocked computer can."

// mayChangeSteps says whether the one signed in may change their second
// steps now: they have none yet, or showed one recently.
func (p *Platform) mayChangeSteps(r *http.Request, t TwoStepSetup) bool {
	return !t.On() || p.stepped.recently(sessionHash(r), p.opts.Now())
}

// Who must use it.

// mustUseTwoStep says whether someone with these abilities must: the owner,
// and anyone who may change things across the whole org.
func mustUseTwoStep(m me) bool {
	if m.Owner {
		return true
	}
	return slices.ContainsFunc([]string{"approve", "hire", "budgets", "invite", "arrange"}, func(a string) bool { return m.Abilities[a].Org })
}

// setUpFirst says whether the one signed in must set up two-step sign-in
// before anything else: the platform requires it, they must, and they
// haven't.
func (p *Platform) setUpFirst(r *http.Request) (bool, error) {
	if !p.opts.RequireTwoStep || p.opts.TwoSteps == nil {
		return false, nil
	}
	s, _ := sessionOf(r)
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), networkOf(r).Name, s.Account)
	if err != nil || t.On() {
		return false, err
	}
	m, err := p.loadMe(r)
	if err != nil {
		return false, err
	}
	return mustUseTwoStep(m), nil
}

// Setting it up, and turning it off, on your account.

type twoStepView struct {
	Secret string
	// Link is the otpauth: address the app adds, and QR it as an image; both
	// made here, not from anything sent, so the page may use them as they are.
	Link, QR template.URL
	Must     bool     // they must set it up
	Adding   bool     // it's on with passkeys; this adds the app beside them
	Codes    []string // recovery codes, shown once, when it's just been set up
}

func (p *Platform) twoStepSetupView(r *http.Request, secret string) twoStepView {
	s, _ := sessionOf(r)
	must, _ := p.setUpFirst(r)
	link := p.totpLink(networkOf(r).Name, s.Account, secret)
	return twoStepView{Secret: secret, Link: template.URL(link), QR: template.URL(qrImage(link)), Must: must}
}

func (p *Platform) twoStepPage(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), networkOf(r).Name, s.Account)
	if err == nil && t.Secret != "" {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	v := p.twoStepSetupView(r, newTOTPSecret())
	v.Adding = t.On()
	p.render(w, r, http.StatusOK, "two-step", withError{View: v})
}

func (p *Platform) setUpTwoStep(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	secret := strings.ToUpper(strings.TrimSpace(r.PostFormValue("secret")))
	if key, err := b32.DecodeString(secret); err != nil || len(key) != 20 {
		p.render(w, r, http.StatusBadRequest, "two-step", withError{Err: "That setup has gone stale. Start again.", View: p.twoStepSetupView(r, newTOTPSecret())})
		return
	}
	step, ok := totpStep(secret, normalCode(r.PostFormValue("code")), p.opts.Now())
	if !ok {
		p.render(w, r, http.StatusBadRequest, "two-step", withError{Err: "That code didn't match. Check the app added this account, then enter the code it shows now.", View: p.twoStepSetupView(r, secret)})
		return
	}
	was, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, s.Account)
	if err == nil && was.Secret != "" {
		p.renderAccount(w, r, http.StatusBadRequest, "Two-step sign-in already uses an authenticator app. To use another, turn it off first, with a code from the one you have.", false)
		return
	}
	if err == nil && !p.mayChangeSteps(r, was) {
		p.renderAccount(w, r, http.StatusForbidden, stepUpNote, false)
		return
	}
	if err == nil && was.On() {
		// Beside their passkeys: the same recovery codes stand.
		was.Secret, was.LastStep = secret, step
		if err := p.opts.TwoSteps.SetTwoStep(r.Context(), n.Name, s.Account, was); err != nil {
			monitor.Problem(p.opts.Logger, "adding %s's authenticator app on %s: %v", s.Account, n.Name, err)
			p.render(w, r, http.StatusInternalServerError, "two-step", withError{Err: "Couldn't save it. Try again in a moment.", View: p.twoStepSetupView(r, secret)})
			return
		}
		p.tellOwner(r.Context(), n, twoStepOnEmail(n, p.networkOrigin(r, n.Name), s.Account, "an authenticator app", false, p.opts.Now()))
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := p.opts.TwoSteps.SetTwoStep(r.Context(), n.Name, s.Account, TwoStepSetup{Secret: secret, LastStep: step, Recovery: hashes, Since: p.opts.Now()}); err != nil {
		monitor.Problem(p.opts.Logger, "setting up %s's two-step sign-in on %s: %v", s.Account, n.Name, err)
		p.render(w, r, http.StatusInternalServerError, "two-step", withError{Err: "Couldn't save it. Try again in a moment.", View: p.twoStepSetupView(r, secret)})
		return
	}
	// Any other sign-in skipped the step it now needs.
	keep := ""
	if st, ok := r.Context().Value(sessionKey).(sessionWithToken); ok {
		keep = st.hash
	}
	p.signOutElsewhere(r.Context(), n, s.Account, keep)
	p.stepped.mark(keep, p.opts.Now()) // they've just shown the app's code
	p.opts.Logger.Printf("%s turned on two-step sign-in on %s", s.Account, n.Name)
	p.tellOwner(r.Context(), n, twoStepOnEmail(n, p.networkOrigin(r, n.Name), s.Account, "an authenticator app", true, p.opts.Now()))
	p.render(w, r, http.StatusOK, "two-step", withError{View: twoStepView{Codes: codes}})
}

func (p *Platform) turnOffTwoStep(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	key := strings.ToLower(n.Name + "|" + s.Account)
	now := p.opts.Now()
	if !p.codeAttempts.Try(key, now) {
		p.renderAccount(w, r, http.StatusTooManyRequests, "Too many wrong codes. Wait 15 minutes, then try again.", false)
		return
	}
	ok, err := p.secondStep(r.Context(), r, n, s.Account, r.PostFormValue("code"))
	if err != nil {
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't check it just now. Try again in a moment.", false)
		return
	}
	if !ok {
		p.renderAccount(w, r, http.StatusUnauthorized, "That code didn't match, so two-step sign-in is still on.", false)
		return
	}
	p.codeAttempts.Reset(key)
	if err := p.opts.TwoSteps.RemoveTwoStep(r.Context(), n.Name, s.Account); err != nil {
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't turn it off. Try again in a moment.", false)
		return
	}
	p.opts.Logger.Printf("%s turned off two-step sign-in on %s", s.Account, n.Name)
	p.tellOwner(r.Context(), n, twoStepOffEmail(n, p.networkOrigin(r, n.Name), s.Account, "turned off by "+s.Account, now))
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// twoStepState is what someone's account page says about their two-step
// sign-in.
type twoStepState struct {
	On       bool
	App      bool // with an authenticator app
	Passkeys []Passkey
	Left     int // recovery codes
	Required bool
}

func (p *Platform) twoStepStateOf(r *http.Request) twoStepState {
	if p.opts.TwoSteps == nil {
		return twoStepState{}
	}
	s, _ := sessionOf(r)
	t, _ := p.opts.TwoSteps.TwoStepOf(r.Context(), networkOf(r).Name, s.Account)
	st := twoStepState{On: t.On(), App: t.Secret != "", Passkeys: t.Passkeys, Left: len(t.Recovery)}
	if p.opts.RequireTwoStep {
		if m, err := p.loadMe(r); err == nil {
			st.Required = mustUseTwoStep(m)
		}
	}
	return st
}

// Resetting it for someone who lost their phone and their codes.

// resetTwoStep turns off account's two-step sign-in on n and signs them out
// of its console everywhere; the owner hears.
func (p *Platform) resetTwoStep(ctx context.Context, r *http.Request, n Network, account, by string) error {
	if err := p.opts.TwoSteps.RemoveTwoStep(ctx, n.Name, account); err != nil {
		return err
	}
	p.signOutEverywhere(ctx, n, account)
	p.opts.Logger.Printf("%s's two-step sign-in on %s was reset by %s", account, n.Name, by)
	p.tellOwner(ctx, n, twoStepOffEmail(n, p.networkOrigin(r, n.Name), account, "reset by "+by, p.opts.Now()))
	return nil
}

// resetSomeonesTwoStep is the owner resetting someone else's, from their
// page in the console.
func (p *Platform) resetSomeonesTwoStep(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	m, err := p.loadMe(r)
	if err != nil {
		p.showPerson(w, r, statusOf(err), messageOf(err))
		return
	}
	account := r.PathValue("account")
	if !m.Owner || strings.EqualFold(account, s.Account) {
		p.showPerson(w, r, http.StatusForbidden, "Only the owner resets someone's two-step sign-in, and not their own here: the owner resets theirs from their networks page on "+p.brand().Name+".")
		return
	}
	if _, err := p.loadPerson(r, account); err != nil {
		p.showPerson(w, r, statusOf(err), messageOf(err))
		return
	}
	if err := p.resetTwoStep(r.Context(), r, networkOf(r), account, s.Account); err != nil {
		monitor.Problem(p.opts.Logger, "resetting %s's two-step sign-in: %v", account, err)
		p.showPerson(w, r, http.StatusInternalServerError, "Couldn't reset it. Try again in a moment.")
		return
	}
	http.Redirect(w, r, "/people/"+url.PathEscape(account), http.StatusSeeOther)
}

// resetOwnersTwoStep is the owner resetting their own from the home page:
// signing in to GuildNet proves who they are, as with their password.
func (p *Platform) resetOwnersTwoStep(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	if p.opts.TwoSteps == nil {
		http.NotFound(w, r)
		return
	}
	if err := p.resetTwoStep(r.Context(), r, n, n.OwnerNick, "the owner, from the "+p.brand().Name+" home page as "+homeOf(r).person.Email); err != nil {
		monitor.Problem(p.opts.Logger, "resetting %s's two-step sign-in: %v", n.Name, err)
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, http.StatusInternalServerError, "home-network", withError{Err: "Couldn't reset it. Try again in a moment.", View: p.view(r, n)})
		return
	}
	http.Redirect(w, r, "/networks/"+n.Name+"?twostep=reset", http.StatusSeeOther)
}

// In memory.

func (s *MemoryStore) TwoStepOf(_ context.Context, network, account string) (TwoStepSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.twoStep[alertKey(network, account)]
	t.Recovery = slices.Clone(t.Recovery)
	t.Passkeys = slices.Clone(s.passkeys[alertKey(network, account)])
	return t, nil
}

func (s *MemoryStore) SetTwoStep(_ context.Context, network, account string, t TwoStepSetup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.twoStep == nil {
		s.twoStep = map[string]TwoStepSetup{}
	}
	t.Recovery, t.Passkeys = slices.Clone(t.Recovery), nil
	s.twoStep[alertKey(network, account)] = t
	return nil
}

func (s *MemoryStore) AddPasskey(_ context.Context, network, account string, k Passkey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passkeys == nil {
		s.passkeys = map[string][]Passkey{}
	}
	key := alertKey(network, account)
	s.passkeys[key] = append(slices.DeleteFunc(slices.Clone(s.passkeys[key]), func(x Passkey) bool { return x.ID == k.ID }), k)
	return nil
}

func (s *MemoryStore) UpdatePasskey(_ context.Context, network, account string, k Passkey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := slices.Clone(s.passkeys[alertKey(network, account)])
	i := slices.IndexFunc(keys, func(x Passkey) bool { return x.ID == k.ID })
	if i < 0 {
		return ErrNotFound
	}
	keys[i].Credential, keys[i].Used = k.Credential, k.Used
	s.passkeys[alertKey(network, account)] = keys
	return nil
}

func (s *MemoryStore) RemovePasskey(_ context.Context, network, account, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := alertKey(network, account)
	s.passkeys[key] = slices.DeleteFunc(slices.Clone(s.passkeys[key]), func(x Passkey) bool { return x.ID == id })
	return nil
}

func (s *MemoryStore) UseStep(_ context.Context, network, account string, step int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.twoStep[alertKey(network, account)]
	if !ok || step <= t.LastStep {
		return false, nil
	}
	t.LastStep = step
	s.twoStep[alertKey(network, account)] = t
	return true, nil
}

func (s *MemoryStore) UseRecovery(_ context.Context, network, account, hash string) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.twoStep[alertKey(network, account)]
	i := slices.Index(t.Recovery, hash)
	if !ok || i < 0 {
		return len(t.Recovery), false, nil
	}
	t.Recovery = slices.Delete(slices.Clone(t.Recovery), i, i+1)
	s.twoStep[alertKey(network, account)] = t
	return len(t.Recovery), true, nil
}

func (s *MemoryStore) RemoveTwoStep(_ context.Context, network, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.twoStep, alertKey(network, account))
	delete(s.passkeys, alertKey(network, account))
	return nil
}

// In Postgres, the app's secret sealed.

func (s *Postgres) TwoStepOf(ctx context.Context, network, account string) (TwoStepSetup, error) {
	var t TwoStepSetup
	var secret []byte
	err := s.pool.QueryRow(ctx, `select secret, last_step, recovery, since from two_step where network = $1 and account = $2`,
		strings.ToLower(network), strings.ToLower(account)).Scan(&secret, &t.LastStep, &t.Recovery, &t.Since)
	if errors.Is(err, pgx.ErrNoRows) {
		t, err = TwoStepSetup{}, nil
	}
	if err != nil {
		return TwoStepSetup{}, err
	}
	if t.Secret, err = s.openString(secret, twoStepSecret(network, account)); err != nil {
		return TwoStepSetup{}, fmt.Errorf("%s's two-step secret on %s: %w", account, network, err)
	}
	t.Since = t.Since.UTC()
	rows, err := s.pool.Query(ctx, `select id, name, credential, added, used from passkeys where network = $1 and account = $2 order by added, id`,
		strings.ToLower(network), strings.ToLower(account))
	if err != nil {
		return TwoStepSetup{}, err
	}
	t.Passkeys, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Passkey, error) {
		var k Passkey
		var used *time.Time
		err := row.Scan(&k.ID, &k.Name, &k.Credential, &k.Added, &used)
		k.Added = k.Added.UTC()
		if used != nil {
			k.Used = used.UTC()
		}
		return k, err
	})
	return t, err
}

func (s *Postgres) AddPasskey(ctx context.Context, network, account string, k Passkey) error {
	_, err := s.pool.Exec(ctx, `insert into passkeys (network, account, id, name, credential, added) values ($1, $2, $3, $4, $5, $6)
		on conflict (network, account, id) do update set name = $4, credential = $5, added = $6`,
		strings.ToLower(network), strings.ToLower(account), k.ID, k.Name, k.Credential, k.Added)
	return err
}

func (s *Postgres) UpdatePasskey(ctx context.Context, network, account string, k Passkey) error {
	tag, err := s.pool.Exec(ctx, `update passkeys set credential = $4, used = $5 where network = $1 and account = $2 and id = $3`,
		strings.ToLower(network), strings.ToLower(account), k.ID, k.Credential, k.Used)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) RemovePasskey(ctx context.Context, network, account, id string) error {
	_, err := s.pool.Exec(ctx, `delete from passkeys where network = $1 and account = $2 and id = $3`, strings.ToLower(network), strings.ToLower(account), id)
	return err
}

func (s *Postgres) SetTwoStep(ctx context.Context, network, account string, t TwoStepSetup) error {
	secret, err := s.sealString(t.Secret, twoStepSecret(network, account))
	if err != nil {
		return err
	}
	t.Recovery = s.keyed(t.Recovery)
	_, err = s.pool.Exec(ctx, `insert into two_step (network, account, secret, last_step, recovery, since) values ($1, $2, $3, $4, $5, $6)
		on conflict (network, account) do update set secret = $3, last_step = $4, recovery = $5, since = $6`,
		strings.ToLower(network), strings.ToLower(account), secret, t.LastStep, t.Recovery, t.Since)
	return err
}

func (s *Postgres) UseStep(ctx context.Context, network, account string, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `update two_step set last_step = $3 where network = $1 and account = $2 and last_step < $3`,
		strings.ToLower(network), strings.ToLower(account), step)
	return err == nil && tag.RowsAffected() == 1, err
}

func (s *Postgres) UseRecovery(ctx context.Context, network, account, hash string) (int, bool, error) {
	// Kept keyed: by the current key, or one from before it changed.
	for _, keyed := range s.sealer.Hashes(hash) {
		var left int
		err := s.pool.QueryRow(ctx, `update two_step set recovery = array_remove(recovery, $3)
			where network = $1 and account = $2 and $3 = any (recovery) returning cardinality(recovery)`,
			strings.ToLower(network), strings.ToLower(account), keyed).Scan(&left)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		return left, err == nil, err
	}
	return 0, false, nil
}

// twoStepSecret is where someone's authenticator secret is kept: theirs,
// on that network, and nobody else's.
func twoStepSecret(network, account string) []byte {
	return aad("two_step", "secret", strings.ToLower(network), strings.ToLower(account))
}

// keyed is hashes as kept: keyed with the current key.
func (s *Postgres) keyed(hashes []string) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, s.sealer.Hash(h))
	}
	return out
}

func (s *Postgres) RemoveTwoStep(ctx context.Context, network, account string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, table := range []string{"two_step", "passkeys"} {
		if _, err := tx.Exec(ctx, `delete from `+table+` where network = $1 and account = $2`, strings.ToLower(network), strings.ToLower(account)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
