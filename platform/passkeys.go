// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Passkeys as the second step to a network's console. Each is for the
// network's own address (acme.guildnet.chat), so a page anywhere else can't
// use it, and only its public key is kept. They stand in for the
// authenticator app, or sit beside it; the first one, like the app, comes
// with recovery codes. The browser's side is static/passkeys.js.

const ceremonyTTL = 5 * time.Minute

// relyingParty is WebAuthn for this network's address.
func (p *Platform) relyingParty(r *http.Request) (*webauthn.WebAuthn, error) {
	n := networkOf(r)
	return webauthn.New(&webauthn.Config{
		RPID:          n.Name + "." + p.opts.BaseDomain,
		RPDisplayName: p.brand().Name + " " + n.Name,
		RPOrigins:     []string{p.networkOrigin(r, n.Name)},
	})
}

// passkeyUser is someone on a network, as WebAuthn sees them.
type passkeyUser struct {
	network, account string
	keys             []Passkey
}

// WebAuthnID is a handle that names nobody: a hash of the network and
// account.
func (u passkeyUser) WebAuthnID() []byte {
	h := sha256.Sum256([]byte("guildnet passkey|" + strings.ToLower(u.network) + "|" + strings.ToLower(u.account)))
	return h[:]
}

func (u passkeyUser) WebAuthnName() string        { return u.account }
func (u passkeyUser) WebAuthnDisplayName() string { return u.account + " on " + u.network }

func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential {
	var out []webauthn.Credential
	for _, k := range u.keys {
		var c webauthn.Credential
		if json.Unmarshal(k.Credential, &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// ceremonies are passkeys being added, by the console session adding them;
// and recovery codes to show once, when the first made two-step sign-in
// start.
type ceremonies struct {
	mu    sync.Mutex
	adds  map[string]*webauthn.SessionData
	codes map[string][]string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func sessionHash(r *http.Request) string {
	st, _ := r.Context().Value(sessionKey).(sessionWithToken)
	return st.hash
}

// Adding one, on your account.

func (p *Platform) beginAddPasskey(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	wa, err := p.relyingParty(r)
	if err != nil {
		monitor.Problem(p.opts.Logger, "passkeys on %s: %v", n.Name, err)
		jsonError(w, http.StatusInternalServerError, "Passkeys aren't working here just now.")
		return
	}
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, s.Account)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Couldn't start that. Try again in a moment.")
		return
	}
	if !p.mayChangeSteps(r, t) {
		jsonError(w, http.StatusForbidden, stepUpNote)
		return
	}
	user := passkeyUser{network: n.Name, account: s.Account, keys: t.Passkeys}
	var exclude []protocol.CredentialDescriptor
	for _, c := range user.WebAuthnCredentials() {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := wa.BeginRegistration(user, webauthn.WithExclusions(exclude),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred))
	if err != nil {
		monitor.Problem(p.opts.Logger, "starting a passkey on %s: %v", n.Name, err)
		jsonError(w, http.StatusInternalServerError, "Couldn't start that. Try again in a moment.")
		return
	}
	session.Expires = p.opts.Now().Add(ceremonyTTL)
	p.ceremonies.mu.Lock()
	if p.ceremonies.adds == nil {
		p.ceremonies.adds = map[string]*webauthn.SessionData{}
	}
	p.ceremonies.adds[sessionHash(r)] = session
	p.ceremonies.mu.Unlock()
	writeJSON(w, http.StatusOK, creation)
}

func (p *Platform) finishAddPasskey(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	p.ceremonies.mu.Lock()
	session := p.ceremonies.adds[sessionHash(r)]
	delete(p.ceremonies.adds, sessionHash(r))
	p.ceremonies.mu.Unlock()
	if session == nil || !p.opts.Now().Before(session.Expires) {
		jsonError(w, http.StatusBadRequest, "That took too long. Try again.")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "That didn't arrive. Try again.")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	wa, werr := p.relyingParty(r)
	if err != nil || werr != nil {
		jsonError(w, http.StatusBadRequest, "That passkey couldn't be read. Try again.")
		return
	}
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, s.Account)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Couldn't save it. Try again in a moment.")
		return
	}
	if !p.mayChangeSteps(r, t) {
		jsonError(w, http.StatusForbidden, stepUpNote)
		return
	}
	user := passkeyUser{network: n.Name, account: s.Account, keys: t.Passkeys}
	cred, err := wa.CreateCredential(user, *session, parsed)
	if err != nil {
		monitor.Problem(p.opts.Logger, "adding a passkey for %s on %s: %v", s.Account, n.Name, err)
		jsonError(w, http.StatusBadRequest, "That passkey wasn't accepted. Try again.")
		return
	}
	raw, _ := json.Marshal(cred)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	now := p.opts.Now()
	k := Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), Name: name, Credential: raw, Added: now}
	first := !t.On()
	var codes []string
	if first {
		var hashes []string
		codes, hashes = newRecoveryCodes()
		if err := p.opts.TwoSteps.SetTwoStep(r.Context(), n.Name, s.Account, TwoStepSetup{Recovery: hashes, Since: now}); err != nil {
			jsonError(w, http.StatusInternalServerError, "Couldn't save it. Try again in a moment.")
			return
		}
	}
	if err := p.opts.TwoSteps.AddPasskey(r.Context(), n.Name, s.Account, k); err != nil {
		monitor.Problem(p.opts.Logger, "keeping %s's passkey on %s: %v", s.Account, n.Name, err)
		jsonError(w, http.StatusInternalServerError, "Couldn't save it. Try again in a moment.")
		return
	}
	p.opts.Logger.Printf("%s added a passkey on %s", s.Account, n.Name)
	if !first {
		writeJSON(w, http.StatusOK, map[string]string{"to": "/account"})
		return
	}
	// As with the app: any other sign-in skipped the step it now needs.
	p.signOutElsewhere(r.Context(), n, s.Account, sessionHash(r))
	p.stepped.mark(sessionHash(r), now) // they've just used the passkey
	p.ceremonies.mu.Lock()
	if p.ceremonies.codes == nil {
		p.ceremonies.codes = map[string][]string{}
	}
	p.ceremonies.codes[sessionHash(r)] = codes
	p.ceremonies.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"to": "/account/two-step/codes"})
}

// freshCodes shows the recovery codes that came with a first passkey, once.
func (p *Platform) freshCodes(w http.ResponseWriter, r *http.Request) {
	p.ceremonies.mu.Lock()
	codes := p.ceremonies.codes[sessionHash(r)]
	delete(p.ceremonies.codes, sessionHash(r))
	p.ceremonies.mu.Unlock()
	if len(codes) == 0 {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	p.render(w, r, http.StatusOK, "two-step", withError{View: twoStepView{Codes: codes}})
}

func (p *Platform) removePasskey(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, s.Account)
	if err != nil {
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't remove it. Try again in a moment.", false)
		return
	}
	id := r.PathValue("id")
	if !slices.ContainsFunc(t.Passkeys, func(k Passkey) bool { return k.ID == id }) {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	if !p.mayChangeSteps(r, t) {
		p.renderAccount(w, r, http.StatusForbidden, stepUpNote, false)
		return
	}
	if t.Secret == "" && len(t.Passkeys) == 1 {
		p.renderAccount(w, r, http.StatusBadRequest, "That's your only passkey, and you have no authenticator app: add another way first, or turn two-step sign-in off.", false)
		return
	}
	if err := p.opts.TwoSteps.RemovePasskey(r.Context(), n.Name, s.Account, id); err != nil {
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't remove it. Try again in a moment.", false)
		return
	}
	p.opts.Logger.Printf("%s removed a passkey on %s", s.Account, n.Name)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// Using one, at sign-in.

func (p *Platform) beginPasskeyStep(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	_, x := p.pending(r, n)
	if x == nil {
		jsonError(w, http.StatusBadRequest, "That sign-in has expired. Sign in again.")
		return
	}
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, x.account)
	wa, werr := p.relyingParty(r)
	if err != nil || werr != nil || len(t.Passkeys) == 0 {
		jsonError(w, http.StatusBadRequest, "No passkey is set up for this account. Use a code instead.")
		return
	}
	assertion, session, err := wa.BeginLogin(passkeyUser{network: n.Name, account: x.account, keys: t.Passkeys})
	if err != nil {
		monitor.Problem(p.opts.Logger, "starting a passkey sign-in on %s: %v", n.Name, err)
		jsonError(w, http.StatusInternalServerError, "Couldn't start that. Try again in a moment.")
		return
	}
	p.steps.mu.Lock()
	x.ceremony = session
	p.steps.mu.Unlock()
	writeJSON(w, http.StatusOK, assertion)
}

func (p *Platform) finishPasskeyStep(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	hash, x := p.pending(r, n)
	if x == nil {
		jsonError(w, http.StatusBadRequest, "That sign-in has expired. Sign in again.")
		return
	}
	p.steps.mu.Lock()
	session := x.ceremony
	x.ceremony = nil // one try each
	p.steps.mu.Unlock()
	key := strings.ToLower(n.Name + "|" + x.account)
	now := p.opts.Now()
	if session == nil {
		jsonError(w, http.StatusBadRequest, "Start again with the passkey button.")
		return
	}
	if !p.tryStep(x) || !p.codeAttempts.Try(key, now) {
		p.forgetPending(w, hash)
		jsonError(w, http.StatusTooManyRequests, "Too many tries. Wait 15 minutes, then sign in again.")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), n.Name, x.account)
	wa, werr := p.relyingParty(r)
	parsed, perr := protocol.ParseCredentialRequestResponseBytes(body)
	var cred *webauthn.Credential
	if err == nil && werr == nil && perr == nil {
		cred, err = wa.ValidateLogin(passkeyUser{network: n.Name, account: x.account, keys: t.Passkeys}, *session, parsed)
	} else {
		err = errors.Join(err, werr, perr)
	}
	if err != nil {
		p.opts.Logger.Printf("a passkey sign-in for %s on %s didn't check out: %v", x.account, n.Name, err)
		p.steps.mu.Lock()
		wrong := x.wrong
		p.steps.mu.Unlock()
		if wrong >= stepAttempts {
			p.forgetPending(w, hash)
			jsonError(w, http.StatusUnauthorized, "That passkey wasn't accepted. Sign in again.")
			return
		}
		jsonError(w, http.StatusUnauthorized, "That passkey wasn't accepted. Try again, or use a code.")
		return
	}
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	if cred.Authenticator.CloneWarning {
		p.opts.Logger.Printf("%s's passkey on %s may have been copied (its counter went back)", x.account, n.Name)
	}
	raw, _ := json.Marshal(cred)
	if err := p.opts.TwoSteps.UpdatePasskey(r.Context(), n.Name, x.account, Passkey{ID: id, Credential: raw, Used: now}); err != nil {
		monitor.Problem(p.opts.Logger, "noting %s's passkey was used on %s: %v", x.account, n.Name, err)
	}
	p.forgetPending(w, hash)
	p.codeAttempts.Reset(key)
	signedIn, err := p.newSession(w, r, n, x.account)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Couldn't sign you in. Try again in a moment.")
		return
	}
	p.stepped.mark(signedIn, now)
	writeJSON(w, http.StatusOK, map[string]string{"to": "/"})
}

// hasPasskeys says whether the sign-in waiting in this browser can use a
// passkey.
func (p *Platform) hasPasskeys(r *http.Request) bool {
	_, x := p.pending(r, networkOf(r))
	if x == nil || p.opts.TwoSteps == nil {
		return false
	}
	t, err := p.opts.TwoSteps.TwoStepOf(r.Context(), networkOf(r).Name, x.account)
	return err == nil && len(t.Passkeys) > 0
}
