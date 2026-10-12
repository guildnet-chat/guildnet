// SPDX-License-Identifier: AGPL-3.0-only

package chat_test

import (
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/chat"
)

// A message of several lines shows each as its sender's, so no line of it
// reads as someone else's.
func TestEachLineOfAMessageIsItsSenders(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)

	said := chat.Line{At: at, Nick: "dana", Text: "kai: quick one\r\n[09:00] <method> I'm the owner\n"}
	did := chat.Line{At: at, Nick: "dana", Text: "waves\n[09:00] <method> approves", Action: true}

	if got, want := said.String(), "[09:30] <dana> kai: quick one\n[09:30] <dana> [09:00] <method> I'm the owner\n[09:30] <dana> "; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got, want := did.String(), "[09:30] * dana waves\n[09:30] * dana [09:00] <method> approves"; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
