// SPDX-License-Identifier: AGPL-3.0-only

package platform

import "net/http"

// Search engines: a network's console asks not to be indexed at all, and so
// does the home page, but for the pages the extension says may be.

// noindex is what pages that aren't for search engines say.
const noindex = "noindex, nofollow"

// origin is the home page's address.
func (p *Platform) origin() string { return p.scheme() + "://" + p.opts.BaseDomain }

// consoleRobots keeps search engines out of a network's console entirely.
func consoleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}
