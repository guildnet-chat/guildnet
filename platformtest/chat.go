// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// ChatToken asks the console for a chat token, as the chat app does, and
// gives it back with the answer.
func (b *Browser) ChatToken() (ChatGrant, Response) {
	b.T.Helper()
	r := b.Do("POST", "/chat/token", nil)
	var tok ChatGrant
	if r.Status == http.StatusOK {
		if err := json.Unmarshal([]byte(r.Body), &tok); err != nil {
			b.T.Fatalf("token: %v: %s", err, r.Body)
		}
	}
	return tok, r
}

// OpenChat opens the chat socket as this browser would.
func (b *Browser) OpenChat(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	origin := b.Origin
	if origin == "" {
		origin = "http://" + b.Host
	}
	header.Set("Origin", origin)
	if b.ForwardedFor != "" {
		header.Set("X-Forwarded-For", b.ForwardedFor)
	}
	for _, c := range b.Cookies {
		header.Add("Cookie", c.Name+"="+c.Value)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(b.W.Server.URL, "http")+"/chat/socket",
		&websocket.DialOptions{Host: b.Host, HTTPHeader: header, Subprotocols: []string{"text.ircv3.net"}})
}

// ChatGrant is what the console hands the chat app to sign in to Ergo with.
type ChatGrant struct {
	Account string `json:"account"`
	Token   string `json:"token"`
}
