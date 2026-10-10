// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"net/http"
	"strings"
)

// Saving in the console says so. A form that went through redirects (303)
// to the page to show next; on the way, it's marked done, and that page
// says "Saved." (or "Done.", for removing something) once. A form that
// didn't go through shows its page again with what's wrong, and isn't
// marked. While a form is on its way, its button shows it (forms.js).

// doneCookie is the mark, until the next page shows it.
func (p *Platform) doneCookie() string { return p.cookieName() + "-done" }

// doneWords are what the next page says, by the mark's value.
var doneWords = map[string]string{"saved": "Saved.", "done": "Done."}

// notMarked are forms whose next page says what happened itself, or that
// aren't saving anything: signing in and out, joining, resets, two-step.
func notMarked(path string) bool {
	for _, prefix := range []string{"/signin", "/signout", "/join/", "/reset/", "/account/two-step", "/account/passkeys/", "/chat/", "/server/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// markDone marks a console form that went through.
func (p *Platform) markDone(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
	if r.Method != http.MethodPost || notMarked(r.URL.Path) {
		return w
	}
	value := "saved"
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		value = "done"
	}
	for _, removal := range []string{"/fire", "/remove", "/cancel", "/delete"} {
		if strings.HasSuffix(r.URL.Path, removal) {
			value = "done"
		}
	}
	return &doneWriter{ResponseWriter: w, cookie: &http.Cookie{Name: p.doneCookie(), Value: value, Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode}}
}

// doneWriter sets the mark if the response is a redirect onwards.
type doneWriter struct {
	http.ResponseWriter
	cookie *http.Cookie
}

func (d *doneWriter) WriteHeader(status int) {
	if status == http.StatusSeeOther {
		http.SetCookie(d.ResponseWriter, d.cookie)
	}
	d.ResponseWriter.WriteHeader(status)
}

func (d *doneWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// doneText is what the page says for a form that went through, if one just
// did; the mark is cleared, so it's said once.
func (p *Platform) doneText(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(p.doneCookie())
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: p.doneCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: p.opts.SecureCookies, SameSite: http.SameSiteLaxMode})
	return doneWords[c.Value]
}
