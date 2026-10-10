// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"net/http"
	"strings"

	"github.com/guildnet-chat/guildnet/netspec"
)

// CertificateCheck answers the edge (Caddy), which asks before getting a
// network's HTTPS certificate on its first visit: GET /check?domain=name.
// It says yes only for networks there are (being set up, or ready), so
// nobody can make the edge ask Let's Encrypt for names at will. It's served
// on its own listener, reachable only from the edge, not on the website.
func (p *Platform) CertificateCheck() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
		domain := strings.ToLower(r.URL.Query().Get("domain"))
		name, ok := strings.CutSuffix(domain, "."+p.opts.BaseDomain)
		// www has its own certificate and redirects to the home page, but
		// matches the networks' wildcard too, so the edge asks about it.
		if ok && name == "www" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if !ok || !netspec.ValidName(name) {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		n, err := p.opts.Networks.Network(r.Context(), name)
		if err != nil || n.Status == Deleting {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
