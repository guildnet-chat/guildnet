// SPDX-License-Identifier: AGPL-3.0-only

package mail

import (
	netmail "net/mail"
	"strings"
)

// Plausible is a light check of an email address someone typed: one
// address, with a dotted domain.
func Plausible(s string) bool {
	if len(s) > 254 {
		return false
	}
	a, err := netmail.ParseAddress(s)
	if err != nil || a.Address != s {
		return false
	}
	_, domain, ok := strings.Cut(s, "@")
	return ok && strings.Contains(domain, ".") && !strings.HasSuffix(domain, ".")
}
