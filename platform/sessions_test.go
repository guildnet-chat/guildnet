// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// A console sign-in ends after a day unused, and a week on however it's
// used; an open chat closes with it.

func bothStores(t *testing.T, test func(t *testing.T, w *World)) {
	t.Run("memory", func(t *testing.T) { test(t, NewWorld(t)) })
	t.Run("postgres", func(t *testing.T) { test(t, NewWorldWithPostgres(t)) })
}

func signedOut(r Response) bool {
	return r.Status == http.StatusSeeOther && strings.HasPrefix(r.Header.Get("Location"), "/signin")
}

func TestASignInEndsAfterADayUnused(t *testing.T) {
	bothStores(t, func(t *testing.T, w *World) {
		ada := w.Browser("acme").SignedIn("ada")
		for range 3 {
			w.Clock.Advance(20 * time.Hour)
			if r := ada.Get("/"); r.Status != http.StatusOK {
				t.Fatalf("used every 20 hours: %d", r.Status)
			}
		}

		w.Clock.Advance(25 * time.Hour)

		if r := ada.Get("/"); !signedOut(r) {
			t.Errorf("after 25 hours unused: %d to %q", r.Status, r.Header.Get("Location"))
		}
	})
}

func TestASignInEndsAWeekOnHoweverItsUsed(t *testing.T) {
	bothStores(t, func(t *testing.T, w *World) {
		ada := w.Browser("acme").SignedIn("ada")
		for range 13 { // six and a half days
			w.Clock.Advance(12 * time.Hour)
			ada.Get("/")
		}

		w.Clock.Advance(13 * time.Hour)

		if r := ada.Get("/"); !signedOut(r) {
			t.Errorf("a week and an hour on: %d", r.Status)
		}
	})
}

func TestAChatClosesWhenItsSignInEnds(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chat, _, err := ada.OpenChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer chat.CloseNow()

	w.Clock.Advance(25 * time.Hour)
	w.RunJobs(time.Minute)

	if _, _, err := chat.Read(ctx); err == nil || ctx.Err() != nil {
		t.Errorf("the chat should close once its sign-in has ended (err %v, ctx %v)", err, ctx.Err())
	}
}
