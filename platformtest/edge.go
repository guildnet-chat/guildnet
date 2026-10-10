// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import "net/netip"

// BehindTheEdge puts the console behind an edge proxy (Caddy, on the
// internet), which terminates TLS and says who the visitor is: browsers
// here send X-Forwarded-For (Browser.ForwardedFor) from the test's own
// address, which the console believes.
func BehindTheEdge(o *Setup) {
	o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
}
