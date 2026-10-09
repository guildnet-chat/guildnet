// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"html"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/platform"
)

// Outbox stands in for Resend: it keeps what was sent, or fails.
type Outbox struct {
	mu   sync.Mutex
	sent []mail.Message
	Fail error
}

func (o *Outbox) Send(_ context.Context, m mail.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Fail != nil {
		return o.Fail
	}
	o.sent = append(o.sent, m)
	return nil
}

// To is what was sent to addr.
func (o *Outbox) To(addr string) []mail.Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	var got []mail.Message
	for _, m := range o.sent {
		if m.To == addr {
			got = append(got, m)
		}
	}
	return got
}

// WithMail sends the console's email to o.
func WithMail(o *Outbox) func(*Setup) {
	return func(opts *Setup) { opts.Mail = o }
}

// Wait is the emails to addr whose subject has about, once there are any
// (some are sent in the background).
func (o *Outbox) Wait(t *testing.T, addr, about string) []mail.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got []mail.Message
		for _, m := range o.To(addr) {
			if strings.Contains(m.Subject, about) {
				got = append(got, m)
			}
		}
		if len(got) > 0 || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One checks there's exactly one email to addr about about, saying says.
func (o *Outbox) One(t *testing.T, addr, about string, says ...string) mail.Message {
	t.Helper()
	got := o.Wait(t, addr, about)
	if len(got) != 1 {
		var subjects []string
		for _, m := range o.To(addr) {
			subjects = append(subjects, m.Subject)
		}
		t.Fatalf("want one email to %s about %q; got %d, of: %q", addr, about, len(got), subjects)
	}
	for _, s := range says {
		if !strings.Contains(got[0].Text, s) || !strings.Contains(html.UnescapeString(got[0].HTML), s) {
			t.Errorf("the email %q should say %q in its text and HTML:\n%s", got[0].Subject, s, got[0].Text)
		}
	}
	return got[0]
}

// None reports whether anything to addr about about arrived in a moment.
func (o *Outbox) None(t *testing.T, addr, about string) bool {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	for _, m := range o.To(addr) {
		if strings.Contains(m.Subject, about) {
			return false
		}
	}
	return true
}

// MailWorld is a world whose console sends email, to the outbox.
func MailWorld(t *testing.T) (*World, *Outbox) {
	out := &Outbox{}
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out)), out
}
