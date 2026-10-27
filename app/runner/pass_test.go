// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// A self-hosted runner signs its desks' egress pass itself: it opens the
// hosts the network's owner let desks reach, on 443, and nothing else. Not
// the network's git hosts: the runner fetches and pushes for desks, which
// reach no git host at all.
func TestTheDesksPassOpensOnlyTheHostsTheOwnerChose(t *testing.T) {
	public, private := sandbox.NewKeys()
	owners := []string{"api.stripe.com", "localhost"}

	pass, err := deskPass(private, "acme", func() []string { return owners }, nil)(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	keys := []ed25519.PublicKey{public}
	if !sandbox.PassOpens(keys, pass, now, "api.stripe.com", 443) {
		t.Error("the pass doesn't open the host the owner chose")
	}
	for _, x := range []struct {
		host string
		port int
	}{{"api.stripe.com", 22}, {"localhost", 443}, {"gitlab.example.com", 443}} {
		if sandbox.PassOpens(keys, pass, now, x.host, x.port) {
			t.Errorf("the pass opens %s:%d", x.host, x.port)
		}
	}
	if none, _ := deskPass(private, "acme", func() []string { return nil }, nil)(context.Background()); none != "" {
		t.Errorf("with no hosts chosen, a pass: %q", none)
	}
}
