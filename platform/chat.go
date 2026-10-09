// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Web chat: the chat app (GuildNet's fork of gamja) at /chat/, talking to the
// network's Ergo through /chat/socket. Signing in to the console is signing
// in to chat: /chat/token hands the app a token that Ergo accepts in place of
// a password (SASL IRCV3BEARER). It lasts only long enough to connect; the
// app asks for a new one each time it connects.

const chatTokenTTL = time.Minute

func (p *Platform) chatToken(w http.ResponseWriter, r *http.Request) {
	s, ok := sessionOf(r)
	if !ok {
		http.Error(w, "Sign in first.", http.StatusUnauthorized)
		return
	}
	if !p.chatAllowed(w, r) {
		return
	}
	n := networkOf(r)
	if n.ChatKey == "" {
		http.Error(w, "Web chat isn't set up on this network yet.", http.StatusServiceUnavailable)
		return
	}
	now := p.opts.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"account": s.Account,
		"aud":     n.Name,
		"iat":     now.Unix(),
		"exp":     now.Add(chatTokenTTL).Unix(),
	}).SignedString([]byte(n.ChatKey))
	if err != nil {
		monitor.Problem(p.opts.Logger, "signing a chat token: %v", err)
		http.Error(w, "Couldn't start chat. Try again in a moment.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{"account": s.Account, "token": token})
}

// chatGuildServ passes what web chat says to GuildServ on to the runner, as
// whoever's signed in here: they've shown their second step, which plain
// IRC never asks for, so GuildServ takes changes from two-step accounts
// this way only. GuildServ answers over IRC, as ever.
func (p *Platform) chatGuildServ(w http.ResponseWriter, r *http.Request) {
	if _, ok := sessionOf(r); !ok {
		http.Error(w, "Sign in first.", http.StatusUnauthorized)
		return
	}
	if !p.chatAllowed(w, r) {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		http.Error(w, "Say what to send GuildServ.", http.StatusBadRequest)
		return
	}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/guildserv", map[string]string{"text": in.Text}, nil); err != nil {
		status := statusOf(err)
		if status < 400 {
			status = http.StatusBadGateway
		}
		http.Error(w, messageOf(err), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatAllowed refuses web chat to someone who must set up two-step sign-in
// first, saying so.
func (p *Platform) chatAllowed(w http.ResponseWriter, r *http.Request) bool {
	first, err := p.setUpFirst(r)
	if err != nil {
		http.Error(w, "The network didn't answer. Try again in a moment.", http.StatusBadGateway)
		return false
	}
	if first {
		http.Error(w, "Set up two-step sign-in first, on your account page.", http.StatusForbidden)
		return false
	}
	return true
}

// chatSocket passes a signed-in person's WebSocket through to their
// network's Ergo, telling it where they really are.
func (p *Platform) chatSocket(w http.ResponseWriter, r *http.Request) {
	if _, ok := sessionOf(r); !ok {
		http.Error(w, "Sign in first.", http.StatusUnauthorized)
		return
	}
	if !p.chatAllowed(w, r) {
		return
	}
	// Browsers always send Origin with a WebSocket, and cookies go with it
	// even from other sites, so this is what stops another page opening
	// someone's chat.
	origin := r.Header.Get("Origin")
	if origin != "https://"+r.Host && origin != "http://"+r.Host {
		http.Error(w, "That came from another site, so it was refused.", http.StatusForbidden)
		return
	}
	n := networkOf(r)
	target, err := url.Parse(n.ChatURL)
	if n.ChatURL == "" || err != nil {
		http.Error(w, "Web chat isn't set up on this network yet.", http.StatusServiceUnavailable)
		return
	}
	target.Scheme = strings.Replace(strings.Replace(target.Scheme, "wss", "https", 1), "ws", "http", 1)
	visitor := p.visitor(r)
	// The socket lives only as long as the sign-in: signing out cancels
	// this, and the proxy then closes both ends.
	s, _ := r.Context().Value(sessionKey).(sessionWithToken)
	ctx, done := p.chats.open(r.Context(), s.hash, chatsOf(n.Name, s.Account))
	defer done()
	r = r.WithContext(ctx)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path, pr.Out.URL.RawPath = target.Path, ""
			pr.Out.Header.Set("X-Forwarded-For", visitor)
			pr.Out.Header.Del("Cookie") // the session is the platform's, not IRC's
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			monitor.Problem(p.opts.Logger, "chat for %s: %v", n.Name, err)
			http.Error(w, "The chat server didn't answer. Try again in a moment.", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// chatPage is chat filling the console: the chat docked beside every page
// (see layout.html) widens to fill it, so it's the same chat, still
// connected, whichever page you're on.
func (p *Platform) chatPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, "chat", nil)
}

// chatApp serves the chat app, and its config, to signed-in people. Only
// this site's own chat page may frame it.
func (p *Platform) chatApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := sessionOf(r); !ok {
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
		return
	}
	if !p.chatAllowed(w, r) {
		return
	}
	if p.opts.ChatDir == "" {
		http.Error(w, "Web chat isn't set up here yet.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Security-Policy", csp("frame-ancestors 'self'"))
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	if r.URL.Path == "/chat/app/config.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// Where the org chart puts this person; the app joins the ones it
		// hasn't put them in before, so leaving a channel sticks.
		// And the repos of those channels' teams, so "PR #42" there links
		// to the right one.
		var mine struct {
			Channels []string            `json:"channels"`
			Repos    map[string][]string `json:"repos"`
		}
		if err := p.runner(r).call(r.Context(), "GET", "/v1/me/channels", nil, &mine); err != nil {
			monitor.Problem(p.opts.Logger, "finding %s's channels: %v", networkOf(r).Name, err)
		}
		if mine.Channels == nil {
			mine.Channels = []string{}
		}
		if mine.Repos == nil {
			mine.Repos = map[string][]string{}
		}
		conf := map[string]any{
			"server": map[string]any{"url": "/chat/socket", "auth": "token", "autoconnect": true, "autojoin": mine.Channels},
			"token":  map[string]any{"url": "/chat/token", "signin": "/signin"},
			// What's said to GuildServ goes this way, not over IRC
			// (chatGuildServ).
			"guildserv": map[string]any{"url": "/chat/guildserv"},
			// Agents (GuildServ gives them ai.<team> and ai-lead.<team>
			// hostnames) are badged AI, and services like GuildServ BOT; the
			// member list reads like the org chart GuildServ keeps in IRC
			// ranks.
			"bots":    map[string]any{"label": "BOT", "labels": map[string]string{"ai.*": "AI", "ai-lead.*": "AI"}},
			"members": map[string]any{"groups": map[string]string{"~": "Owner", "@": "Leads", "+": "Team", "": "Everyone else"}},
			"repos":   mine.Repos,
		}
		if s := p.chatSource(); s != "" {
			conf["source"] = s
		}
		json.NewEncoder(w).Encode(conf)
		return
	}
	if r.URL.Path == "/chat/app/version" {
		p.chatVersion(w, r)
		return
	}
	if r.URL.Path == "/chat/app/approvals" {
		p.chatApprovals(w, r)
		return
	}
	if r.URL.Path == "/chat/app/" || r.URL.Path == "/chat/app/index.html" {
		p.chatIndex(w, r)
		return
	}
	http.StripPrefix("/chat/app", http.FileServerFS(os.DirFS(p.opts.ChatDir))).ServeHTTP(w, r)
}

// chatSource is where the chat app's source is: the commit being served, when
// the build says which, or else the repository; "" when it isn't published.
func (p *Platform) chatSource() string {
	if p.opts.ChatSource == "" {
		return ""
	}
	if p.opts.ChatCommit == "" {
		return p.opts.ChatSource
	}
	return strings.TrimSuffix(p.opts.ChatSource, "/") + "/tree/" + p.opts.ChatCommit
}

// chatApprovals says which approvals are waiting on the signed-in person, and
// when that was so: the chat takes Approve and Deny off older ones that
// aren't, whose outcome may not be in the history it loaded. Numbers only.
func (p *Platform) chatApprovals(w http.ResponseWriter, r *http.Request) {
	at := p.opts.Now()
	list, err := p.loadApprovals(r)
	if err != nil {
		monitor.Problem(p.opts.Logger, "approvals waiting, for %s's chat: %v", networkOf(r).Name, err)
		http.Error(w, "The network didn't answer. Try again in a moment.", http.StatusBadGateway)
		return
	}
	ids := []int64{}
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"waiting": ids, "at": at.UTC().Format(time.RFC3339Nano)})
}

// chatIndex serves the chat app's page with GuildNet's design system laid
// over its own styles: tokens, fonts, the chat theme, and the light and dark
// switch the console uses.
func (p *Platform) chatIndex(w http.ResponseWriter, r *http.Request) {
	page, err := os.ReadFile(filepath.Join(p.opts.ChatDir, "index.html"))
	if err != nil {
		monitor.Problem(p.opts.Logger, "reading the chat app: %v", err)
		http.Error(w, "Web chat isn't set up here yet.", http.StatusNotFound)
		return
	}
	var extra strings.Builder
	// Which version this is: the app asks for the current one now and then
	// (version.go), and offers to reload when they differ.
	fmt.Fprintf(&extra, `<meta name="guildnet-version" content="%s">`, p.version)
	for _, s := range []string{"tokens.css", "chat.css"} {
		u, _ := p.static(s)
		fmt.Fprintf(&extra, `<link rel="stylesheet" href="%s">`, u)
	}
	u, _ := p.static("theme.js")
	fmt.Fprintf(&extra, `<script src="%s"></script>`, u)
	html := string(page)
	at := strings.Index(html, "</head>")
	if at < 0 {
		at = strings.Index(html, "<body")
	}
	if at < 0 {
		at = len(html)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	io.WriteString(w, html[:at]+extra.String()+html[at:])
}

// openChats are the chat sockets open under each sign-in, so signing out
// can close them.
type openChats struct {
	mu        sync.Mutex
	next      int
	bySession map[string]map[int]context.CancelFunc
}

// open registers a socket under a sign-in, and under whose it is (keys:
// the sign-in's hash, and chatsOf's); done unregisters it.
func (c *openChats) open(parent context.Context, keys ...string) (ctx context.Context, done func()) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bySession == nil {
		c.bySession = map[string]map[int]context.CancelFunc{}
	}
	id := c.next
	c.next++
	for _, k := range keys {
		if c.bySession[k] == nil {
			c.bySession[k] = map[int]context.CancelFunc{}
		}
		c.bySession[k][id] = cancel
	}
	return ctx, func() {
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, k := range keys {
			delete(c.bySession[k], id)
			if len(c.bySession[k]) == 0 {
				delete(c.bySession, k)
			}
		}
	}
}

// chatsOf is the key every chat socket of account's on network is also
// kept under, so taking them out (or a new password) closes them all.
func chatsOf(network, account string) string {
	return "account\x00" + strings.ToLower(network) + "\x00" + strings.ToLower(account)
}

// closeAll closes every socket open under a sign-in.
func (c *openChats) closeAll(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.bySession[session] {
		cancel()
	}
}
