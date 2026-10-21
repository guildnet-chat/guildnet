// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"net/http"
	"net/url"
	"regexp"
)

var (
	appPasswordShown = regexp.MustCompile(`<code class="app-password">([^<]+)</code>`)
	appPasswordForm  = regexp.MustCompile(`action="/account/app-passwords/([a-z0-9]+)/revoke"`)
)

// MakeAppPassword makes an IRC app password called name for whoever's
// signed in, on their account page: its id, and the password shown once.
func (b *Browser) MakeAppPassword(name string) (id, password string) {
	b.T.Helper()
	b.Get("/account")
	r := b.Post("/account/app-passwords", url.Values{"name": {name}})
	shown := appPasswordShown.FindStringSubmatch(r.Body)
	forms := appPasswordForm.FindAllStringSubmatch(r.Body, -1)
	if r.Status != http.StatusOK || shown == nil || len(forms) == 0 {
		b.T.Fatalf("making an app password: %d\n%s", r.Status, r.Text())
	}
	return forms[len(forms)-1][1], shown[1]
}
