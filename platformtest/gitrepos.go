// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"

	"github.com/guildnet-chat/guildnet/internal/gitremote"
	"github.com/guildnet-chat/guildnet/internal/gitwire"
	"github.com/guildnet-chat/guildnet/platform"
)

// storeGitRepos is a network's git repositories as its runner hears of
// them, read from the console's store (a real runner asks the broker,
// which reads the same store).
type storeGitRepos struct {
	store   platform.GitRepos
	network string
}

func (s storeGitRepos) Repos() []gitremote.Repo {
	repos, _ := s.store.GitRepos(context.Background(), s.network)
	var out []gitremote.Repo
	for _, r := range repos {
		out = append(out, gitremote.Repo{Name: r.Name, URL: r.URL, Read: r.Read != (gitwire.Credential{}), Write: r.Write != (gitwire.Credential{})})
	}
	return out
}

func (storeGitRepos) Refresh(context.Context) error { return nil }
