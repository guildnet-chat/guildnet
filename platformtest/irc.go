// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"
)

// SayOnIRC has nick say text in channel on network n's IRC server, as a
// person in an IRC app would.
func SayOnIRC(t *testing.T, n *Network, nick, channel, text string) {
	t.Helper()
	c := &ircevent.Connection{Server: n.IRC.Addr(), Nick: nick, User: nick, RealName: nick}
	joined := make(chan struct{})
	var once sync.Once
	c.AddConnectCallback(func(ircmsg.Message) { c.Join(channel) })
	c.AddCallback("JOIN", func(e ircmsg.Message) {
		if strings.EqualFold(e.Nick(), c.CurrentNick()) {
			once.Do(func() { close(joined) })
		}
	})
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	go c.Loop()
	t.Cleanup(c.Quit)
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never joined %s", nick, channel)
	}
	c.Privmsg(channel, text)
}

var checklistItem = regexp.MustCompile(`(?s)<li class="step( is-done)?"[^>]*data-step="([a-z]+)"`)

// Checklist is the org page's getting-started steps: step -> done.
func Checklist(b *Browser) map[string]bool {
	out := map[string]bool{}
	for _, m := range checklistItem.FindAllStringSubmatch(b.Get("/").Body, -1) {
		out[m[2]] = m[1] != ""
	}
	return out
}
