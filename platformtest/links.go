// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var (
	JoinLink  = regexp.MustCompile(`/join/([A-Za-z0-9_-]{20,})`)
	ResetLink = regexp.MustCompile(`/reset/([A-Za-z0-9_-]{20,})`)
)

// AskForLink asks for a sign-in email to email, and gives back the link's
// path and the code in it.
func (b *Browser) AskForLink(out *Outbox, email string) (string, string) {
	b.T.Helper()
	asked := email
	email = strings.ToLower(email) // where it's sent
	before := len(out.To(email))
	b.Get("/signin")
	r := b.Post("/signin/email", url.Values{"email": {asked}})
	if r.Status != http.StatusOK || !strings.Contains(r.Text(), "Check your email") {
		b.T.Fatalf("asking for a link: %d %s", r.Status, r.Text())
	}
	sent := out.To(email)
	if len(sent) != before+1 {
		b.T.Fatalf("%d sign-in emails to %s, want one more than %d", len(sent), email, before)
	}
	m := sent[len(sent)-1]
	link, code := signInLink.FindStringSubmatch(m.Text), signInCode.FindStringSubmatch(m.Text)
	if link == nil || code == nil || !strings.Contains(m.HTML, code[1]) {
		b.T.Fatalf("the email should carry a link and a code, in text and HTML:\n%s", m.Text)
	}
	return link[1], code[1]
}

var (
	signInLink = regexp.MustCompile(`http://` + regexp.QuoteMeta(BaseDomain) + `(/signin/email/[A-Za-z0-9_-]+)`)
	signInCode = regexp.MustCompile(`code is (\d{6})`)
)
