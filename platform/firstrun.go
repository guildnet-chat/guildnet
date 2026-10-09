// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/limit"
	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/netspec"
)

// First-run setup: a self-hosted install runs one network, made once, by
// whoever has the setup code the console prints to its log at start. Its
// IRC server and runner are already running (the install's stack starts
// them); setup gives the runner its settings, through the broker, and makes
// the owner's IRC account. The owner is that account: there's nobody on
// the home page.

// FirstRun is a self-hosted install's one network, as the install's
// settings describe it before setup makes it.
type FirstRun struct {
	// Network is its name, fixed at install: its IRC server, address and
	// certificate are already named for it.
	Network string
	// Addresses are where its runner, IRC server and web chat are.
	Addresses netspec.Addresses
	// Its secrets, which the install's settings give its runner and Ergo
	// too.
	RunnerToken, ErgoToken, ChatKey string
}

// firstRun is setup's state: the code's hash (only while there's a code)
// and the browser setting up. Neither is kept anywhere else: a restart
// before setup's done prints a new code.
type firstRun struct {
	mu      sync.Mutex
	code    string // the code's hash
	browser string // the setup cookie's hash
	until   time.Time
	guesses *limit.Tries // wrong codes, by visitor
	one     sync.Mutex   // one setup at a time
}

const (
	// setupGuesses wrong codes from an address within setupWindow, and the
	// next are refused until the window passes.
	setupGuesses = 5
	setupWindow  = 15 * time.Minute
	// setupBrowserTTL is how long the browser that started setting up may
	// follow it and be handed in.
	setupBrowserTTL = time.Hour
	// codeLetters are the setup code's: no 0 or O, 1, I or L, to misread.
	codeLetters = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
)

// fixedHost is a self-hosted install's "host": its one network is already
// running, so putting it only says where it is (its runner fetches its
// settings from the broker), and nothing can be removed, stopped or
// started from here.
type fixedHost struct{ fr *FirstRun }

func (h fixedHost) Put(ctx context.Context, name string, s netspec.Spec) (netspec.Addresses, error) {
	if !strings.EqualFold(name, h.fr.Network) {
		return netspec.Addresses{}, errors.New("this install runs only " + h.fr.Network)
	}
	// A runner already going asks the broker again now, and restarts if
	// its settings changed (setup was tried again with others).
	refreshRunner(ctx, h.fr.Addresses.RunnerURL, s.RunnerToken)
	return h.fr.Addresses, nil
}

func (h fixedHost) Remove(context.Context, string) error {
	return errors.New("a self-hosted network is removed with its install")
}
func (h fixedHost) Stop(context.Context, string) error {
	return errors.New("a self-hosted network is stopped with its install")
}
func (h fixedHost) Start(context.Context, string) error {
	return errors.New("a self-hosted network is started with its install")
}
func (h fixedHost) Stats(context.Context) (netspec.Stats, error) { return netspec.Stats{}, nil }

// refreshRunner asks the runner at url to fetch its settings and keys
// again, if it's there.
func refreshRunner(ctx context.Context, url, token string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(url, "/")+"/v1/model-keys/refresh", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// startFirstRun takes the network's addresses and secrets from the
// install's settings, if setup has made it, and prints a new setup code,
// unless it's set up.
func (p *Platform) startFirstRun() {
	p.firstRun.guesses = limit.New(setupGuesses, setupWindow)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := p.opts.Networks.Network(ctx, p.opts.FirstRun.Network)
	if err == nil {
		n = p.reapplySettings(ctx, n)
	}
	if err == nil && n.Status == Ready {
		return
	}
	code := newSetupCode()
	p.firstRun.code = hashToken(strings.ReplaceAll(code, "-", ""))
	p.opts.Logger.Printf("first-run setup: open %s://%s/ and enter the setup code %s (a new one each time this starts, until %s is set up)",
		p.scheme(), p.opts.BaseDomain, code, p.opts.FirstRun.Network)
}

// reapplySettings gives the install's network the addresses and secrets
// the install's settings say it has now, if they've changed since setup (the
// install moved, from Compose to a cloud, say): its secrets are sealed again
// to the new addresses.
func (p *Platform) reapplySettings(ctx context.Context, n Network) Network {
	fr := p.opts.FirstRun
	was := n
	n.RunnerURL, n.ErgoURL, n.ChatURL = fr.Addresses.RunnerURL, fr.Addresses.ErgoURL, fr.Addresses.ChatURL
	n.RunnerToken, n.ErgoToken, n.ChatKey = fr.RunnerToken, fr.ErgoToken, fr.ChatKey
	if n.RunnerURL == was.RunnerURL && n.ErgoURL == was.ErgoURL && n.ChatURL == was.ChatURL &&
		n.RunnerToken == was.RunnerToken && n.ErgoToken == was.ErgoToken && n.ChatKey == was.ChatKey {
		return was
	}
	if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
		monitor.Problem(p.opts.Logger, "network %s: taking its addresses from the install's settings: %v", n.Name, err)
		return was
	}
	p.opts.Logger.Printf("network %s: its addresses or secrets in the install's settings changed: the console uses the new ones", n.Name)
	return n
}

// newSetupCode is twelve letters and digits, in threes of four: about 59
// bits, and guessing is slowed down.
func newSetupCode() string {
	var b strings.Builder
	for i := range 12 {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeLetters))))
		if err != nil {
			panic(err)
		}
		b.WriteByte(codeLetters[n.Int64()])
	}
	return b.String()
}

// rightCode says whether code is the setup code (written any way: lower
// case, spaces, dashes).
func (p *Platform) rightCode(code string) bool {
	code = strings.ToUpper(strings.Join(strings.FieldsFunc(code, func(r rune) bool { return r == '-' || r == ' ' }), ""))
	p.firstRun.mu.Lock()
	defer p.firstRun.mu.Unlock()
	return p.firstRun.code != "" && code != "" && subtle.ConstantTimeCompare([]byte(hashToken(code)), []byte(p.firstRun.code)) == 1
}

// setupState is how far setting the network up has got.
type setupState int

const (
	notSetUp setupState = iota // nothing yet, or it didn't finish
	settingUp
	setUp
)

// firstRunNetwork is the install's network, if setup has made it, and how
// far it's got.
func (p *Platform) firstRunNetwork(ctx context.Context) (Network, setupState, error) {
	n, err := p.opts.Networks.Network(ctx, p.opts.FirstRun.Network)
	switch {
	case errors.Is(err, ErrNotFound):
		return Network{}, notSetUp, nil
	case err != nil:
		return Network{}, notSetUp, err
	case n.Status == Ready:
		return n, setUp, nil
	case n.Status == Creating && p.opts.Now().Sub(n.Updated) <= setupTimeout:
		return n, settingUp, nil
	}
	return n, notSetUp, nil
}

func (p *Platform) setupCookie() string { return p.cookieName() + "-setup" }

// settingUpHere says whether this is the browser that started setting up.
func (p *Platform) settingUpHere(r *http.Request) bool {
	c, err := r.Cookie(p.setupCookie())
	if err != nil || c.Value == "" {
		return false
	}
	p.firstRun.mu.Lock()
	defer p.firstRun.mu.Unlock()
	return p.firstRun.browser != "" && p.opts.Now().Before(p.firstRun.until) &&
		subtle.ConstantTimeCompare([]byte(hashToken(c.Value)), []byte(p.firstRun.browser)) == 1
}

// setupView is the setup page.
type setupView struct {
	newNetworkView
	Network, Address string
	State            string // "form", "setting-up", "ready"
	Problem          string // why the last try didn't finish
	OwnerNick        string
	CanEmail         bool
}

func (p *Platform) setupView(r *http.Request, n Network, state setupState, form map[string]string) setupView {
	v := setupView{newNetworkView: p.newNetworkForm(r, form), Network: p.opts.FirstRun.Network, OwnerNick: n.OwnerNick,
		Address:  strings.TrimPrefix(strings.TrimPrefix(p.networkOrigin(r, p.opts.FirstRun.Network), "https://"), "http://"),
		CanEmail: p.opts.Mail != nil && p.opts.Alerts != nil}
	switch state {
	case setUp:
		v.State = "ready"
	case settingUp:
		v.State = "setting-up"
	default:
		v.State, v.Problem = "form", n.Problem
		if n.Status == Creating {
			v.Problem = "Setting it up stopped part way."
		}
	}
	return v
}

// setupPage is the home page on a self-hosted install: setup until it's
// done, then the way to the console.
func (p *Platform) setupPage(w http.ResponseWriter, r *http.Request) {
	n, state, err := p.firstRunNetwork(r.Context())
	if err != nil {
		monitor.Problem(p.opts.Logger, "first-run setup: %v", err)
		http.Error(w, "Something went wrong. Try again in a moment.", http.StatusInternalServerError)
		return
	}
	if state == setUp && !p.settingUpHere(r) {
		http.Redirect(w, r, p.networkOrigin(r, n.Name)+"/", http.StatusSeeOther)
		return
	}
	// "Open" goes on to the console, and it may arrive (as setting up
	// finishes) on this page loaded before: browsers hold a form's redirect
	// to the policy of the page it's on, whatever the page shows now.
	w.Header().Set("Content-Security-Policy", csp("form-action 'self' "+p.networkOrigin(r, p.opts.FirstRun.Network)))
	p.renderHome(w, r, http.StatusOK, "home-setup", withError{View: p.setupView(r, n, state, nil)})
}

// firstRunSetUp is setup's form: with the code, it makes the network and
// starts setting it up.
func (p *Platform) firstRunSetUp(w http.ResponseWriter, r *http.Request) {
	fr := p.opts.FirstRun
	f := networkForm{
		name: fr.Network, template: r.PostFormValue("template"), timezone: strings.TrimSpace(r.PostFormValue("timezone")),
		nick: strings.TrimSpace(r.PostFormValue("nick")), password: r.PostFormValue("password"),
		provider: r.PostFormValue("provider"), key: strings.TrimSpace(r.PostFormValue("model_key")),
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	form := map[string]string{"template": f.template, "timezone": f.timezone, "nick": f.nick, "provider": f.provider, "email": email}
	again := func(status int, n Network, msg string) {
		v := p.setupView(r, n, notSetUp, form)
		v.Problem = ""
		p.renderHome(w, r, status, "home-setup", withError{Err: msg, View: v})
	}
	visitor, now := p.visitor(r), p.opts.Now()
	if !p.firstRun.guesses.Try(visitor, now) {
		again(http.StatusTooManyRequests, Network{}, "Too many wrong setup codes from here. Try again in 15 minutes.")
		return
	}
	// One at a time, so it can't be set up twice by asking twice at once.
	p.firstRun.one.Lock()
	defer p.firstRun.one.Unlock()
	n, state, err := p.firstRunNetwork(r.Context())
	if err != nil {
		monitor.Problem(p.opts.Logger, "first-run setup: %v", err)
		again(http.StatusInternalServerError, n, "Something went wrong. Try again in a moment.")
		return
	}
	if state != notSetUp {
		http.Error(w, fr.Network+" is already set up.", http.StatusConflict)
		return
	}
	if !p.rightCode(r.PostFormValue("code")) {
		p.opts.Logger.Printf("first-run setup: a wrong setup code was tried from %s", visitor)
		again(http.StatusForbidden, n, "That isn't the setup code. It's in the console's log: docker compose logs platform.")
		return
	}
	msg := f.checkSettings()
	switch {
	case msg != "":
	case f.password != r.PostFormValue("confirm"):
		msg = "The two passwords are different."
	case email != "" && !mail.Plausible(email):
		msg = "That doesn't look like an email address."
	}
	prov, _ := providerNamed(f.provider)
	if msg == "" {
		msg = p.checkKey(r.Context(), prov, f.key)
	}
	if msg != "" {
		again(http.StatusBadRequest, n, msg)
		return
	}
	var keys ModelKeys
	keys.Set(prov, f.key)
	made := n.Name != ""
	n = Network{
		Name: fr.Network, Region: p.opts.Region, Status: Creating, OwnerNick: f.nick,
		Template: f.template, Timezone: f.timezone, Keys: keys, Updated: now,
		RunnerToken: fr.RunnerToken, ErgoToken: fr.ErgoToken, ChatKey: fr.ChatKey,
	}
	if made {
		err = p.opts.Networks.UpdateNetwork(r.Context(), n)
	} else {
		err = p.opts.Networks.AddNetwork(r.Context(), n)
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "first-run setup: making %s: %v", n.Name, err)
		again(http.StatusInternalServerError, n, "Couldn't set it up. Try again in a moment.")
		return
	}
	p.opts.Logger.Printf("first-run setup: setting %s up, owned by %s", n.Name, n.OwnerNick)
	// This browser follows it, and is handed in.
	token := newToken()
	p.firstRun.mu.Lock()
	p.firstRun.browser, p.firstRun.until = hashToken(token), now.Add(setupBrowserTTL)
	p.firstRun.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: p.setupCookie(), Value: token, Path: "/", MaxAge: int(setupBrowserTTL.Seconds()),
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	origin := p.networkOrigin(r, n.Name)
	if email != "" && p.opts.Mail != nil && p.opts.Alerts != nil {
		p.askToConfirm(r, n, f.nick, email, origin)
	}
	go p.setUp(n.Name, f.password, origin)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// askToConfirm sends the owner's alert address its confirmation link.
func (p *Platform) askToConfirm(r *http.Request, n Network, account, email, origin string) {
	token := newToken()
	if err := p.opts.Alerts.AwaitConfirmation(r.Context(), n.Name, account, email, hashToken(token), p.opts.Now().Add(confirmTTL)); err != nil {
		monitor.Problem(p.opts.Logger, "keeping %s's alert address on %s: %v", account, n.Name, err)
		return
	}
	if err := p.opts.Mail.Send(r.Context(), p.message(confirmEmail(email, account, n.Name, origin+"/account/email/confirm/"+token))); err != nil {
		monitor.Problem(p.opts.Logger, "emailing %s's confirmation link on %s: %v", account, n.Name, err)
	}
}

// openFirst hands the browser that set the network up into its console, as
// its owner, once.
func (p *Platform) openFirst(w http.ResponseWriter, r *http.Request) {
	n, state, err := p.firstRunNetwork(r.Context())
	if err != nil || state != setUp || !p.settingUpHere(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p.firstRun.mu.Lock()
	p.firstRun.browser = ""
	p.firstRun.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: p.setupCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	code := newToken()
	p.handoffs.add(code, handoff{network: n.Name, account: n.OwnerNick, expires: p.opts.Now().Add(handoffTTL)})
	http.Redirect(w, r, p.networkOrigin(r, n.Name)+"/welcome?code="+code, http.StatusSeeOther)
}

// networkSettings is what the broker tells a self-hosted network's runner
// once setup has made it: what it was made from, and who owns it.
func (p *Platform) networkSettings(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(r)
	if !ok {
		if fr := p.opts.FirstRun; fr != nil && strings.EqualFold(r.Header.Get("GuildNet-Network"), fr.Network) {
			if _, err := p.opts.Networks.Network(r.Context(), fr.Network); errors.Is(err, ErrNotFound) {
				brokerError(w, http.StatusNotFound, "the network isn't set up yet: first-run setup gives its settings")
				return
			}
		}
		brokerError(w, http.StatusUnauthorized, "the network's secret is required")
		return
	}
	if n.OwnerNick == "" || n.Template == "" {
		brokerError(w, http.StatusNotFound, "the network isn't set up yet: first-run setup gives its settings")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]string{"template": n.Template, "owner": n.OwnerNick, "company": n.Name, "timezone": n.Timezone})
}

// ownerResetBy is who made an owner's reset link: never the console, which
// makes none for the owner (makeResetLink), only the server's command line.
const ownerResetBy = "whoever runs the server"

// ownerResetTTL is how long an owner's reset link lasts.
const ownerResetTTL = time.Hour

// ResetOwner makes a link for a self-hosted network's owner who's locked
// out (no password, no second step): used once, within the hour, it sets a
// new password and turns their two-step sign-in off until they set it up
// again. It's for the server's command line (reset-owner): whoever can run
// that has the install already, so it opens no new way in.
func ResetOwner(ctx context.Context, opts Options) (string, error) {
	if opts.FirstRun == nil || opts.Networks == nil || opts.Links == nil {
		return "", errors.New("only a self-hosted install's owner is reset this way")
	}
	n, err := opts.Networks.Network(ctx, opts.FirstRun.Network)
	if errors.Is(err, ErrNotFound) || err == nil && n.OwnerNick == "" {
		return "", errors.New(opts.FirstRun.Network + " isn't set up yet, so it has no owner: set it up at its address first")
	}
	if err != nil {
		return "", err
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	token := newToken()
	l := Link{ID: newID(), Network: n.Name, Kind: Reset, Account: n.OwnerNick, CreatedBy: ownerResetBy, Created: now, Expires: now.Add(ownerResetTTL)}
	if err := opts.Links.AddLink(ctx, hashToken(token), l); err != nil {
		return "", err
	}
	scheme := "http"
	if opts.SecureCookies {
		scheme = "https"
	}
	return scheme + "://" + n.Name + "." + opts.BaseDomain + "/reset/" + token, nil
}
