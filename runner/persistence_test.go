// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/guildnet-chat/guildnet/internal/ircfake"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/pgtest"
	"github.com/guildnet-chat/guildnet/runner"
)

// With Postgres, org changes made through GuildServ survive a restart.

func TestOrgChangesSurviveARestart(t *testing.T) {
	pg, err := pgtest.Start()
	if errors.Is(err, pgtest.ErrNoDocker) {
		t.Skip("Docker isn't running; skipping Postgres")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Stop)
	url, err := pg.NewDatabase()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ircfake.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.AddAccount(ownerAccount, testPassword)
	cfg, seed, l := testOrgSeed(t, srv.Addr())
	n := &network{t: t, srv: srv}

	start := func(seed org.Seed) (*runner.Runner, *org.Postgres) {
		store, err := org.OpenPostgres(context.Background(), url, seed)
		if err != nil {
			t.Fatal(err)
		}
		r := runner.New(cfg, store, &fakeBrain{reply: replyWith("back")}, l, log.New(io.Discard, "", 0))
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		return r, store
	}

	r, store := start(seed)
	owner := joinHumanLoggedIn(t, srv.Addr(), ownerAccount, ownerAccount, ownerPassword, "#general", "#engineering")
	n.human = owner
	n.waitForMembers("#engineering", "kai", "ledger", "method", "vera")
	owner.say("GuildServ", `HIRE rex ROLE "Support" TEAM Engineering`)
	owner.expect("rex hired", guildServSaying("method", "hired rex"))
	owner.say("GuildServ", "PROMOTE rex")
	owner.expect("rex promoted", guildServSaying("method", "is now lead"))
	owner.say("GuildServ", "DEMOTE vera")
	owner.expect("vera demoted", guildServSaying("method", "no longer leads"))
	owner.say("GuildServ", "FIRE kai")
	owner.expect("kai fired", guildServSaying("method", "fired kai"))

	r.Stop()
	store.Close()
	n.waitForAbsent("#engineering", "rex")

	// A fresh runner and database connection, as after a deploy.
	r2, store2 := start(seed)
	t.Cleanup(func() { r2.Stop(); store2.Close() })

	n.waitForMembers("#engineering", "ledger", "method", "rex", "vera")
	owner.say("GuildServ", "TEAMS")
	owner.expect("rex still leading Engineering", guildServSaying("method", "Engineering (#engineering) led by rex"))
}
