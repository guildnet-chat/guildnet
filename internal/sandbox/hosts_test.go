// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// A network's owner chooses more hosts its desks may reach, by name: never
// an address, nothing inside a network, and nothing GuildNet's blocklist
// names (places to drop what was taken), whatever the owner says.

func TestAHostDesksMayReachIsAPublicName(t *testing.T) {
	for in, want := range map[string]string{
		"api.stripe.com":         "api.stripe.com",
		"  API.Stripe.COM. ":     "api.stripe.com",
		"xn--bcher-kva.example1": "xn--bcher-kva.example1",
		"crates.io":              "crates.io",
	} {
		if got, problem := sandbox.CheckDeskHost(in, nil); got != want || problem != "" {
			t.Errorf("%q: %q, %q; want %q", in, got, problem, want)
		}
	}
}

func TestAHostDesksMayReachIsNeverAnAddressOrInsideANetwork(t *testing.T) {
	for _, in := range []string{
		"", "1.2.3.4", "127.1", "2130706433", "[::1]", "::1", "0x7f.0.0.1",
		"localhost", "intranet", "metadata.google.internal", "printer.local", "nas.lan", "box.home.arpa", "app.localhost",
		"api.stripe.com:8443", "https://api.stripe.com", "api.stripe.com/v1", "*.stripe.com", "user@api.stripe.com",
		"-bad.example.com", "bad-.example.com", "a..b.com", "under_score.example.com", strings.Repeat("a", 64) + ".com",
	} {
		if got, problem := sandbox.CheckDeskHost(in, nil); problem == "" {
			t.Errorf("%q was taken, as %q", in, got)
		}
	}
}

func TestTheBlocklistWinsWhateverTheOwnerSays(t *testing.T) {
	for _, in := range []string{"pastebin.com", "webhook.site", "abc.ngrok.io", "transfer.sh", "x.trycloudflare.com"} {
		if _, problem := sandbox.CheckDeskHost(in, nil); !strings.Contains(problem, "blocked") {
			t.Errorf("%q: %q, want it blocked by GuildNet's own list", in, problem)
		}
	}
	// An install adds its own; an entry blocks the name and every name
	// under it.
	if _, problem := sandbox.CheckDeskHost("files.example.org", []string{"example.org"}); !strings.Contains(problem, "blocked") {
		t.Errorf("an install's own blocklist: %q", problem)
	}
	if _, problem := sandbox.CheckDeskHost("notexample.org", []string{"example.org"}); problem != "" {
		t.Errorf("a name that only ends the same way: %q", problem)
	}
}
