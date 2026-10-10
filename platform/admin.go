// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"time"
)

// Admin pages (Access Admins): the extension's, on the base domain, for
// those its IsAdmin says are admins. To everyone else they aren't there at
// all.

// adminPage is an admin page (Admins): only for admins, and a form that
// went through says it was done.
func (p *Platform) adminPage(h http.HandlerFunc) http.HandlerFunc {
	guarded := p.admin(h)
	return func(w http.ResponseWriter, r *http.Request) { guarded(p.markDone(w, r), r) }
}

// admin serves h only to an admin; to anyone else there's nothing here.
// Changes must carry the session's CSRF token.
func (p *Platform) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := p.loadHomeSession(r)
		if !ok || !p.admits(s.person) {
			http.NotFound(w, r)
			return
		}
		if p.homeSetUpFirst(r.Context(), s.person) {
			http.Redirect(w, r, "/account/two-step", http.StatusSeeOther)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" &&
			subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
			http.Error(w, "That form has expired. Go back, reload the page, and try again.", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), homeKey, s)))
	}
}

// For pages' templates.

// megabytes is a size in MB (MiB, as Docker counts).
func megabytes(b int64) string { return fmt.Sprintf("%d MB", (b+(1<<19))>>20) }

// when is a moment, briefly, in UTC; "never" for none.
func when(t any) string {
	var at time.Time
	switch v := t.(type) {
	case time.Time:
		at = v
	case *time.Time:
		if v != nil {
			at = *v
		}
	}
	if at.IsZero() {
		return "never"
	}
	if at.UTC().Year() != time.Now().UTC().Year() {
		return at.UTC().Format("Jan 2 2006")
	}
	return at.UTC().Format("Jan 2, 15:04 UTC")
}

// count is how many of something: 1 agent, 3 agents.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
