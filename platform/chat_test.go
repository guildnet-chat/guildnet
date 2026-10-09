// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"github.com/guildnet-chat/guildnet/internal/ergotest"
	"github.com/guildnet-chat/guildnet/platform"
)

// Web chat: the chat app (a gamja fork) at name.<base>/chat/, talking to that
// network's Ergo through the platform. Signing in to the console is signing
// in to chat: the console hands the app a short-lived token that Ergo
// accepts in place of a password.

func claimsOf(t *testing.T, token, key string) jwt.MapClaims {
	t.Helper()
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return []byte(key), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		t.Fatalf("the token doesn't check out: %v", err)
	}
	return claims
}

func TestSignedInPeopleGetAShortLivedChatToken(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	tok, r := ada.ChatToken()

	if r.Status != http.StatusOK || tok.Account != "ada" {
		t.Fatalf("token: %d %s", r.Status, r.Body)
	}
	claims := claimsOf(t, tok.Token, ChatKeys["acme"])
	if claims["account"] != "ada" {
		t.Errorf("account claim = %v, want ada", claims["account"])
	}
	if aud, _ := claims.GetAudience(); len(aud) != 1 || aud[0] != "acme" {
		t.Errorf("audience = %v; the token should be for acme only", aud)
	}
	exp, _ := claims.GetExpirationTime()
	if left := time.Until(exp.Time); left <= 0 || left > 2*time.Minute {
		t.Errorf("the token lasts %v; it only needs to last long enough to connect", left)
	}
	if !strings.Contains(r.Header.Get("Cache-Control"), "no-store") {
		t.Error("tokens mustn't be cached")
	}
}

func TestNoChatTokenWithoutSigningIn(t *testing.T) {
	w := NewWorld(t)

	_, r := w.Browser("acme").ChatToken()

	if r.Status != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", r.Status)
	}
}

func TestAChatTokenIsOnlyGoodOnItsOwnNetwork(t *testing.T) {
	w := NewWorld(t)
	tok, _ := w.Browser("acme").SignedIn("ada").ChatToken()

	_, err := jwt.Parse(tok.Token, func(*jwt.Token) (any, error) { return []byte(ChatKeys["globex"]), nil })

	if err == nil {
		t.Error("acme's token was accepted with globex's key")
	}
}

func TestChatTokensCantBeFetchedFromAnotherSite(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Origin = "http://globex." + BaseDomain

	_, r := ada.ChatToken()

	if r.Status != http.StatusForbidden {
		t.Errorf("status %d, want 403", r.Status)
	}
}

func TestTheChatSocketReachesYourNetworksIRCServer(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := ada.OpenChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.Write(ctx, websocket.MessageText, []byte("PING hello"))
	_, echo, err := c.Read(ctx)

	if err != nil || string(echo) != "PING hello" {
		t.Fatalf("round trip: %q, %v", echo, err)
	}
	if got := w.Chat.Connections(); len(got) != 1 || got[0].Network != "acme" {
		t.Errorf("connections: %+v, want one to acme's IRC server", got)
	}
}

// Signing out of the console signs you out of chat: any chat open under
// that sign-in is cut off.
func TestSigningOutClosesChat(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	elsewhere := w.Browser("acme").SignedIn("ada") // another sign-in stays
	ada.Get("/")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mine, _, err := ada.OpenChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer mine.CloseNow()
	other, _, err := elsewhere.OpenChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.CloseNow()

	ada.Post("/signout", nil)

	if _, _, err := mine.Read(ctx); err == nil || ctx.Err() != nil {
		t.Errorf("ada's chat should close when she signs out (err %v, ctx %v)", err, ctx.Err())
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	other.Write(ctx2, websocket.MessageText, []byte("PING still here"))
	if _, echo, err := other.Read(ctx2); err != nil || string(echo) != "PING still here" {
		t.Errorf("a chat under another sign-in should carry on: %q, %v", echo, err)
	}
}

// Web chat sends what's said to GuildServ through the console, as whoever's
// signed in there, their second step shown: GuildServ takes changes from
// two-step accounts that way, never from plain IRC.
func TestWebChatSendsGuildServThroughTheConsole(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/")

	r := ada.Call("/chat/guildserv", []byte(`{"text":"HIRE rex ROLE \"Support\" TEAM Engineering"}`))

	if r.Status != http.StatusNoContent {
		t.Fatalf("sending: %d %s", r.Status, r.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := w.Networks["acme"].Store.Agent(context.Background(), "rex"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("GuildServ never hired rex")
		}
	}
	if r := w.Browser("acme").Call("/chat/guildserv", []byte(`{"text":"FIRE kai"}`)); r.Status != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", r.Status)
	}
}

// The broker tells a network's runner how someone signs in: whether two
// steps are wanted of the org's owner and admins, and whether they've
// turned them on.
func TestTheBrokerSaysHowSomeoneSignsIn(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, RequireTwoStep)
	cy := w.Browser("acme").SignedIn("cy")
	cy.SetUpTwoStep()
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	n, _ := w.Store.Network(context.Background(), "acme")
	ask := func(account, secret string) (int, map[string]bool) {
		req, _ := http.NewRequest("GET", broker.URL+"/two-step?account="+account, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("GuildNet-Network", "acme")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]bool
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if status, out := ask("cy", n.RunnerToken); status != http.StatusOK || !out["required"] || !out["on"] {
		t.Errorf("cy: %d %v, want required and on", status, out)
	}
	if _, out := ask("ada", n.RunnerToken); !out["required"] || out["on"] {
		t.Errorf("ada: %v, want required, not on", out)
	}
	if status, _ := ask("cy", "wrong"); status != http.StatusUnauthorized {
		t.Errorf("without the network's secret: %d", status)
	}
}

// Turning on two-step signs you out everywhere else, chats and all: someone
// who signed in with only the password can't stay in through an open chat.
func TestTurningOnTwoStepClosesChatsElsewhere(t *testing.T) {
	for how, turnOn := range map[string]func(w *World, b *Browser){
		"an app":    func(_ *World, b *Browser) { b.SetUpTwoStep() },
		"a passkey": func(w *World, b *Browser) { b.AddPasskey(NewAuthenticator(w.T, "acme"), "/account/two-step").To(w.T) },
	} {
		t.Run(how, func(t *testing.T) {
			w := NewWorld(t)
			intruder := w.Browser("acme").SignedIn("cy")
			cy := w.Browser("acme").SignedIn("cy")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			theirs, _, err := intruder.OpenChat(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer theirs.CloseNow()

			turnOn(w, cy)

			if _, _, err := theirs.Read(ctx); err == nil || ctx.Err() != nil {
				t.Errorf("the other sign-in's chat should close (err %v, ctx %v)", err, ctx.Err())
			}
		})
	}
}

func TestTheChatSocketIsOnlyForSignedInPeople(t *testing.T) {
	w := NewWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := w.Browser("acme").OpenChat(ctx)

	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("opening chat signed out: %v, %v; want 401", resp, err)
	}
	if len(w.Chat.Connections()) != 0 {
		t.Error("nothing should reach the IRC server")
	}
}

func TestOtherSitesCantOpenYourChat(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.Origin = "http://globex." + BaseDomain // even another network here
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := ada.OpenChat(ctx)

	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a page on another site opened ada's chat: %v, %v", resp, err)
	}
	if len(w.Chat.Connections()) != 0 {
		t.Error("nothing should reach the IRC server")
	}
}

func TestIRCSeesWhereTheVisitorIsNotThePlatform(t *testing.T) {
	w := newWorldBehindTheEdge(t)
	ada := w.Browser("acme").SignedIn("ada")
	ada.ForwardedFor = "10.1.1.1, 203.0.113.5" // the first part is made up
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := ada.OpenChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.CloseNow()

	if got := w.Chat.Connections(); len(got) != 1 || got[0].ForwardedFor != "203.0.113.5" {
		t.Errorf("IRC was told %+v, want exactly the visitor's address", got)
	}
}

func TestTheChatAppIsServedToSignedInPeople(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>gamja</title>"), 0o644)
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.ChatDir = dir })

	signedOut := w.Browser("acme").Get("/chat/app/")
	ada := w.Browser("acme").SignedIn("ada")
	app := ada.Get("/chat/app/")
	conf := ada.Get("/chat/app/config.json")

	if signedOut.Status != http.StatusSeeOther || signedOut.Header.Get("Location") != "/signin" {
		t.Errorf("signed out: %d %q, want off to sign in", signedOut.Status, signedOut.Header.Get("Location"))
	}
	if app.Status != http.StatusOK || !strings.Contains(app.Body, "gamja") {
		t.Errorf("the app: %d", app.Status)
	}
	var c struct {
		Server struct {
			URL         string `json:"url"`
			Auth        string `json:"auth"`
			Autoconnect bool   `json:"autoconnect"`
		} `json:"server"`
		Token struct {
			URL    string `json:"url"`
			Signin string `json:"signin"`
		} `json:"token"`
		Bots struct {
			Label  string            `json:"label"`
			Labels map[string]string `json:"labels"`
		} `json:"bots"`
		Members struct {
			Groups map[string]string `json:"groups"`
		} `json:"members"`
	}
	if err := json.Unmarshal([]byte(conf.Body), &c); err != nil {
		t.Fatalf("config.json: %v: %s", err, conf.Body)
	}
	// Agents (hostnames ai.<team> and ai-lead.<team>) are badged AI and
	// services like GuildServ BOT; the member list is grouped the way the org
	// chart shows in IRC: the owner (~), team leads (@), the team (+).
	if c.Bots.Label != "BOT" || c.Bots.Labels["ai.*"] != "AI" || c.Bots.Labels["ai-lead.*"] != "AI" || c.Members.Groups["~"] != "Owner" || c.Members.Groups["@"] != "Leads" || c.Members.Groups["+"] != "Team" {
		t.Errorf("bots and members: %+v %+v", c.Bots, c.Members)
	}
	if c.Server.URL != "/chat/socket" || c.Server.Auth != "token" || !c.Server.Autoconnect || c.Token.URL != "/chat/token" || c.Token.Signin != "/signin" {
		t.Errorf("config.json = %+v", c)
	}
}

// The chat app is AGPL, so its users are offered its source: config.json says
// where, the exact commit served when the build knows it, and the chat's
// settings link there. Until a source is set there is no link.
func TestTheChatSaysWhereItsSourceIs(t *testing.T) {
	source := func(o func(*Setup)) (string, bool) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>gamja</title>"), 0o644)
		w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(opts *Setup) {
			opts.ChatDir = dir
			o(opts)
		})
		var c map[string]any
		body := w.Browser("acme").SignedIn("ada").Get("/chat/app/config.json").Body
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("config.json: %v: %s", err, body)
		}
		s, ok := c["source"].(string)
		_, present := c["source"]
		return s, ok || present
	}

	if s, ok := source(func(o *Setup) {}); ok {
		t.Errorf("no source set, but config.json says %q", s)
	}
	if s, ok := source(func(o *Setup) { o.ChatCommit = "0123456789ab" }); ok {
		t.Errorf("a commit but nowhere to find it, and config.json says %q", s)
	}
	if s, _ := source(func(o *Setup) {
		o.ChatSource = "https://github.com/guildnet-chat/guildnet-chat"
		o.ChatCommit = "0123456789abcdef0123456789abcdef01234567"
	}); s != "https://github.com/guildnet-chat/guildnet-chat/tree/0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("source = %q, want the commit served", s)
	}
	if s, _ := source(func(o *Setup) { o.ChatSource = "https://github.com/guildnet-chat/guildnet-chat/" }); s != "https://github.com/guildnet-chat/guildnet-chat/" {
		t.Errorf("source without a commit = %q, want the repository", s)
	}
}

func newWorldWithChatApp(t *testing.T) *World {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!DOCTYPE html><meta charset=utf-8><title>gamja</title><link rel=stylesheet href=gamja.css><script type=module src=gamja.js></script><body><p>app</p>"), 0o644)
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) { o.ChatDir = dir })
}

// Chat opens inside the console, like any other page: the sidebar stays,
// and the chat app fills the rest.
func TestChatOpensInsideTheConsole(t *testing.T) {
	w := newWorldWithChatApp(t)

	r := w.Browser("acme").SignedIn("ada").Get("/chat/")

	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	for _, want := range []string{`href="/approvals"`, `<iframe`, `src="/chat/app/"`, `aria-current="page"><svg`} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("the chat page should include %q", want)
		}
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self'") {
		t.Errorf("CSP %q should let the page frame the chat app", csp)
	}
}

func TestOnlyTheConsoleMayFrameTheChatApp(t *testing.T) {
	w := newWorldWithChatApp(t)
	ada := w.Browser("acme").SignedIn("ada")

	app := ada.Get("/chat/app/")
	org := ada.Get("/")

	if csp := app.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("the chat app's CSP %q should allow only this site to frame it", csp)
	}
	if app.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options %q", app.Header.Get("X-Frame-Options"))
	}
	// Console pages frame the chat docked beside them, from this site only,
	// and nothing may frame them.
	if csp := org.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "frame-src 'self'") {
		t.Errorf("console pages should stay unframeable and frame only this site: %q", csp)
	}
	// The public pages frame nothing.
	if csp := w.Home().Get("/").Header.Get("Content-Security-Policy"); strings.Contains(csp, "frame-src") {
		t.Errorf("the landing page may frame things: %q", csp)
	}
}

// The chat app wears GuildNet's design system (tokens, fonts, light and
// dark, following the console's switch), laid over its own styles, with
// nothing added to the fork itself.
func TestTheChatAppWearsTheDesignSystem(t *testing.T) {
	w := newWorldWithChatApp(t)

	app := w.Browser("acme").SignedIn("ada").Get("/chat/app/")

	head, _, ok := strings.Cut(app.Body, "<body") // it's minified: no <head> tags
	if !ok {
		t.Fatalf("no head:\n%s", app.Body)
	}
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`<link rel="stylesheet" href="/static/tokens.css\?v=[0-9a-f]+">`),
		regexp.MustCompile(`<link rel="stylesheet" href="/static/chat.css\?v=[0-9a-f]+">`),
		regexp.MustCompile(`<script src="/static/theme.js\?v=[0-9a-f]+"></script>`),
	} {
		if !want.MatchString(head) {
			t.Errorf("the chat app's head should have %s", want)
		}
	}
	if strings.Index(head, "chat.css") < strings.Index(head, "gamja.css") {
		t.Error("the design system should come after the app's own styles, so it wins")
	}
}

// Chat puts you in your channels: the ones the org chart gives you, the
// same as GuildServ's ranks in IRC.
func TestChatJoinsYourChannels(t *testing.T) {
	w := newWorldWithChatApp(t)
	autojoin := func(account string) []string {
		t.Helper()
		r := w.Browser("acme").SignedIn(account).Get("/chat/app/config.json")
		var c struct {
			Server struct {
				Autojoin []string `json:"autojoin"`
			} `json:"server"`
		}
		if err := json.Unmarshal([]byte(r.Body), &c); err != nil {
			t.Fatalf("config.json: %v: %s", err, r.Body)
		}
		return c.Server.Autojoin
	}

	ada, cy := autojoin("ada"), autojoin("cy")

	if !slices.Contains(ada, "#engineering") || !slices.Contains(ada, "#sales") {
		t.Errorf("ada owns acme, so every channel: %v", ada)
	}
	if !slices.Contains(cy, "#engineering") || slices.Contains(cy, "#sales") {
		t.Errorf("cy is in Engineering, not Sales: %v", cy)
	}
}

// Chat links "PR #42" in a team's channel to the team's repo, so it's told
// the repos of your channels.
func TestChatKnowsYourChannelsRepos(t *testing.T) {
	w := newWorldWithChatApp(t)

	r := w.Browser("acme").SignedIn("cy").Get("/chat/app/config.json")

	var c struct {
		Repos map[string][]string `json:"repos"`
	}
	if err := json.Unmarshal([]byte(r.Body), &c); err != nil {
		t.Fatalf("config.json: %v: %s", err, r.Body)
	}
	if len(c.Repos) != 1 || !slices.Equal(c.Repos["#engineering"], []string{"acme-inc/web"}) {
		t.Errorf("cy's channels' repos = %v, want #engineering's acme-inc/web", c.Repos)
	}
}

// What became of an older approval may not be in the chat's history, so it
// asks which are still waiting on the signed-in person, to take the buttons
// off the ones that aren't.
func TestChatAsksWhichApprovalsAreWaitingOnYou(t *testing.T) {
	w := newWorldWithChatApp(t)
	id := w.Networks["acme"].RaiseApproval(t, "kai", "ada", "merge PR #17 into main")
	ada := w.Browser("acme").SignedIn("ada")
	waiting := func(b *Browser) []int64 {
		t.Helper()
		r := b.Get("/chat/app/approvals")
		var out struct {
			Waiting []int64 `json:"waiting"`
			At      string  `json:"at"`
		}
		if err := json.Unmarshal([]byte(r.Body), &out); err != nil || r.Status != http.StatusOK || out.At == "" {
			t.Fatalf("approvals: %d %v %s", r.Status, err, r.Body)
		}
		return out.Waiting
	}

	if got := waiting(ada); !slices.Equal(got, []int64{id}) {
		t.Errorf("waiting on ada: %v, want [%d]", got, id)
	}
	if got := waiting(w.Browser("acme").SignedIn("cy")); len(got) != 0 {
		t.Errorf("waiting on cy, who can't decide it: %v", got)
	}
	ada.Get("/approvals")
	if r := ada.Post("/approvals/"+strconv.FormatInt(id, 10)+"/approve", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("approving: %d %s", r.Status, r.Text())
	}
	if got := waiting(ada); len(got) != 0 {
		t.Errorf("once approved: %v", got)
	}
	if r := w.Browser("acme").Get("/chat/app/approvals"); r.Status == http.StatusOK {
		t.Errorf("signed out: %d", r.Status)
	}
}

func TestSignedOutChatGoesToSignIn(t *testing.T) {
	w := newWorldWithChatApp(t)

	r := w.Browser("acme").Get("/chat/")

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("%d %q", r.Status, r.Header.Get("Location"))
	}
}

func TestTheConsoleLinksToChat(t *testing.T) {
	w := NewWorld(t)

	r := w.Browser("acme").SignedIn("ada").Get("/")

	if !strings.Contains(r.Body, `href="/chat/"`) {
		t.Error("the console should link to chat")
	}
}

// authenticateLines sends a SASL response in 400-byte pieces, ending with
// "+" when the last piece is exactly 400.
func authenticateLines(payload string) []string {
	enc := base64.StdEncoding.EncodeToString([]byte(payload))
	var out []string
	for len(enc) >= 400 {
		out = append(out, "AUTHENTICATE "+enc[:400])
		enc = enc[400:]
	}
	if enc == "" {
		return append(out, "AUTHENTICATE +")
	}
	return append(out, "AUTHENTICATE "+enc)
}

// The whole way through, with a real Ergo: a token from the console signs
// you in to IRC over the platform's socket.
func TestAChatTokenSignsYouInToIRC(t *testing.T) {
	ergo, err := ergotest.Start(ergotest.WithJWTAudience("acme"))
	if errors.Is(err, ergotest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping real-Ergo tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ergo.Stop)
	if err := ergo.Register("ada", "ada-pw"); err != nil {
		t.Fatal(err)
	}
	s := platform.NewMemoryStore()
	w := NewWorldWith(t, func() Store { return s })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	acme, _ := s.Network(ctx, "acme")
	acme.ChatURL, acme.ChatKey = ergo.WebSocketURL, ergo.JWTKey
	s.PutNetwork(ctx, acme)
	ada := w.Browser("acme").SignedIn("ada")
	tok, _ := ada.ChatToken()
	c, _, err := ada.OpenChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	lines := []string{"CAP LS 302", "CAP REQ :sasl", "NICK ada", "USER ada 0 * :ada", "AUTHENTICATE IRCV3BEARER"}
	lines = append(lines, authenticateLines("\x00jwt\x00"+tok.Token)...)
	for _, line := range append(lines, "CAP END") {
		c.Write(ctx, websocket.MessageText, []byte(line))
	}
	offered := false
	for {
		_, msg, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("never logged in: %v", err)
		}
		// The chat app only uses a way of signing in that Ergo offers.
		if strings.Contains(string(msg), " CAP * LS ") && strings.Contains(string(msg), "IRCV3BEARER") {
			offered = true
		}
		fields := strings.Fields(string(msg))
		if len(fields) > 1 && fields[1] == "904" {
			t.Fatalf("Ergo refused the token: %s", msg)
		}
		if len(fields) > 4 && fields[1] == "900" {
			if fields[4] != "ada" {
				t.Errorf("logged in as %s, want ada", fields[4])
			}
			if !offered {
				t.Error("Ergo should offer IRCV3BEARER in CAP LS, or the chat app won't use it")
			}
			return
		}
	}
}
