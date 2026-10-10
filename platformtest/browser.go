// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"html"
	"regexp"
	"strings"
	"time"
)

// Eventually fetches path until its page contains want.
func (b *Browser) Eventually(path, want string) Response {
	b.T.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := b.Get(path)
		if strings.Contains(r.Text(), want) || time.Now().After(deadline) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// VisibleText is a page's words as a reader sees them: without tags, and
// with runs of spaces as one.
func VisibleText(r Response) string {
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(r.Body, "")
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}
