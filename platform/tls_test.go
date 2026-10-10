// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The edge (Caddy) gets each network's HTTPS certificate the first time
// someone visits it, after asking the platform whether that address is a
// network. It says yes only for networks there are, so nobody can make it
// ask Let's Encrypt for names at will.

func TestTheEdgeGetsCertificatesOnlyForNetworksThereAre(t *testing.T) {
	s := platform.NewMemoryStore()
	ctx := context.Background()
	s.AddNetwork(ctx, platform.Network{Name: "acme", Status: platform.Ready, RunnerToken: "x", ErgoToken: "y"})
	s.AddNetwork(ctx, platform.Network{Name: "initech", Status: platform.Creating, RunnerToken: "x", ErgoToken: "y"})
	s.AddNetwork(ctx, platform.Network{Name: "globex", Status: platform.Deleting, RunnerToken: "x", ErgoToken: "y"})
	p := platform.New(platform.Options{BaseDomain: "guildnet.test", Networks: s, Sessions: s, People: s,
		Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(p.CertificateCheck())
	t.Cleanup(srv.Close)

	for domain, want := range map[string]int{
		"acme.guildnet.test":      http.StatusOK,
		"ACME.guildnet.test":      http.StatusOK,
		"initech.guildnet.test":   http.StatusOK, // it's being set up
		"www.guildnet.test":       http.StatusOK, // redirected to the home page
		"globex.guildnet.test":    http.StatusForbidden,
		"nobody.guildnet.test":    http.StatusForbidden,
		"a.b.guildnet.test":       http.StatusForbidden,
		"acme.guildnet.test.evil": http.StatusForbidden,
		"evil.example":            http.StatusForbidden,
		"":                        http.StatusForbidden,
	} {
		resp, err := http.Get(srv.URL + "/check?domain=" + domain)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%q: %d, want %d", domain, resp.StatusCode, want)
		}
	}
}

func TestTheCertificateCheckIsntOnTheWebsite(t *testing.T) {
	w := NewWorld(t)
	for _, b := range []*Browser{w.Home(), w.Browser("acme")} {
		if r := b.Get("/check?domain=acme." + BaseDomain); r.Status == http.StatusOK {
			t.Errorf("%s answers the certificate check", b.Host)
		}
	}
}
