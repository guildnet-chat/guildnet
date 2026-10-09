// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// On the internet the platform sits behind an edge proxy (Caddy), which
// terminates TLS and tells it who the visitor is. What's limited for each
// visitor (here, sign-in emails asked for on the home page) is limited by
// who they really are.

func newWorldBehindTheEdge(t *testing.T) *World {
	return NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, BehindTheEdge)
}

func TestBehindTheEdgeEachVisitorHasTheirOwnSignInEmailLimit(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, BehindTheEdge, WithMail(out))
	flooder, neighbour := w.Home(), w.Home()
	flooder.ForwardedFor, neighbour.ForwardedFor = "203.0.113.5", "198.51.100.7"
	flooder.Get("/signin")
	for i := range 20 {
		flooder.Post("/signin/email", url.Values{"email": {"dev" + strconv.Itoa(i) + "@example.com"}})
	}

	if r := flooder.Post("/signin/email", url.Values{"email": {"late@example.com"}}); r.Status != http.StatusTooManyRequests {
		t.Errorf("the flooder: %d, want 429", r.Status)
	}
	neighbour.Get("/signin")
	if r := neighbour.Post("/signin/email", url.Values{"email": {"neighbour@example.com"}}); r.Status != http.StatusOK {
		t.Errorf("someone else: %d; one visitor's flood shouldn't lock everyone out", r.Status)
	}
}

func TestVisitorsCantForgeWhereTheyComeFromForSignInEmails(t *testing.T) {
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, BehindTheEdge, WithMail(out))
	v := w.Home()
	v.Get("/signin")
	for i := range 20 {
		// The edge appends the real address to whatever the visitor sent.
		v.ForwardedFor = "10.0.0." + strconv.Itoa(i) + ", 203.0.113.5"
		v.Post("/signin/email", url.Values{"email": {"dev" + strconv.Itoa(i) + "@example.com"}})
	}
	v.ForwardedFor = "10.9.9.9, 203.0.113.5"

	if r := v.Post("/signin/email", url.Values{"email": {"late@example.com"}}); r.Status != http.StatusTooManyRequests {
		t.Errorf("%d: a made-up address in front of the real one dodged the limit", r.Status)
	}
}

func TestWithoutAnEdgeTheForwardedHeaderIsIgnoredForSignInEmails(t *testing.T) {
	w, _ := MailWorld(t) // trusts no proxy
	v := w.Home()
	v.Get("/signin")
	for i := range 20 {
		v.ForwardedFor = "203.0.113." + strconv.Itoa(i)
		v.Post("/signin/email", url.Values{"email": {"dev" + strconv.Itoa(i) + "@example.com"}})
	}
	v.ForwardedFor = "203.0.113.99"

	if r := v.Post("/signin/email", url.Values{"email": {"late@example.com"}}); r.Status != http.StatusTooManyRequests {
		t.Errorf("%d: a header anyone can send dodged the limit", r.Status)
	}
}
