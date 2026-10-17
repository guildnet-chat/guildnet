// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"context"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/github"
)

// The team's private repos are one source and its public repos another. A
// public repo carries what people outside the company wrote (a pull request
// from a fork, its title, an issue), so work read in a private repo goes to
// a public one only with a person's approval, and what a public repo says
// goes into a private one the same way. Within either kind, it flows freely.
func TestPrivateAndPublicReposAreSourcesOfTheirOwn(t *testing.T) {
	f := newFakeGitHub(t, "guildnet", "guildnet-chat")
	c, err := github.New(github.Options{AppID: 1, InstallationID: 2, Key: testKey(t), Owner: owner,
		Repos: []string{"guildnet", "guildnet-chat"}, Public: []string{"guildnet-chat"}, BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	read := func(repo string) map[string]any { return map[string]any{"repo": repo, "path": "README.md"} }
	comment := func(repo string) map[string]any {
		return map[string]any{"repo": repo, "number": float64(1), "body": "Looks good."}
	}

	for _, c2 := range []struct {
		name        string
		read, write string
		asks        bool
	}{
		{"private, then public", "guildnet", "guildnet-chat", true},
		{"public, then private", "guildnet-chat", "guildnet", true},
		{"private, then private", "guildnet", "guildnet", false},
		{"public, then public", "guildnet-chat", "guildnet-chat", false},
	} {
		tools := agent.Tracked(c.WithoutWorkspace())
		if _, err := tools.Run(context.Background(), kai, "github_read_file", read(c2.read)); err != nil {
			t.Fatalf("%s: reading %s: %v", c2.name, c2.read, err)
		}

		asks, why := tools.(agent.Asker).MustAsk(kai, "github_comment", comment(c2.write))

		if asks != c2.asks {
			t.Errorf("%s: commenting asks: %v (%q), want %v", c2.name, asks, why, c2.asks)
		}
	}
}
