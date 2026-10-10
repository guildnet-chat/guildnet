// SPDX-License-Identifier: AGPL-3.0-only

package mail_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/mail"
)

// resend is a stand-in for Resend's API: it keeps what it was sent, and
// answers with status (200 unless set).
type resend struct {
	status int
	answer string
	got    struct {
		path, auth, key string
		body            map[string]any
	}
}

func (f *resend) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got.path, f.got.auth, f.got.key = r.Method+" "+r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		json.NewDecoder(r.Body).Decode(&f.got.body)
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(f.answer))
			return
		}
		w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func sender(url string) mail.Resend {
	return mail.Resend{Key: "re_test_key", From: "GuildNet <hello@mail.guildnet.test>", ReplyTo: "hello@guildnet.test", URL: url}
}

// GuildNet's email goes out through Resend, from GuildNet's address with
// replies to its inbox, as plain text; a message sent twice with the same
// key (a retry) is sent once.
func TestAnEmailIsSentThroughResend(t *testing.T) {
	f := &resend{}
	s := sender(f.start(t))

	err := s.Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Your GuildNet beta invite", Text: "Hi,\n\nYou're invited.", Key: "beta-invite/eve"})

	if err != nil {
		t.Fatal(err)
	}
	if f.got.path != "POST /emails" || f.got.auth != "Bearer re_test_key" || f.got.key != "beta-invite/eve" {
		t.Errorf("request: %q, auth %q, idempotency key %q", f.got.path, f.got.auth, f.got.key)
	}
	want := map[string]any{"from": "GuildNet <hello@mail.guildnet.test>", "to": []any{"eve@example.com"}, "reply_to": "hello@guildnet.test",
		"subject": "Your GuildNet beta invite", "text": "Hi,\n\nYou're invited."}
	for k, v := range want {
		if b, _ := json.Marshal(f.got.body[k]); string(b) != mustJSON(v) {
			t.Errorf("%s = %s, want %s", k, b, mustJSON(v))
		}
	}
	if _, html := f.got.body["html"]; html {
		t.Error("without an HTML copy, the email should be plain text only")
	}
}

// An email with an HTML copy sends both: mail apps show whichever they can.
func TestAnHTMLCopyGoesWithTheText(t *testing.T) {
	f := &resend{}
	s := sender(f.start(t))

	if err := s.Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi", HTML: "<p>Hi</p>"}); err != nil {
		t.Fatal(err)
	}

	if f.got.body["html"] != "<p>Hi</p>" || f.got.body["text"] != "Hi" {
		t.Errorf("html %v, text %v: want both", f.got.body["html"], f.got.body["text"])
	}
}

// A refusal says why, in Resend's words, and never carries the key.
func TestARefusalSaysWhy(t *testing.T) {
	f := &resend{status: http.StatusUnprocessableEntity, answer: `{"statusCode":422,"name":"validation_error","message":"The mail.guildnet.test domain is not verified."}`}
	s := sender(f.start(t))

	err := s.Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi"})

	if err == nil || !strings.Contains(err.Error(), "not verified") || !strings.Contains(err.Error(), "422") {
		t.Errorf("err = %v, want Resend's reason and status", err)
	}
	if err != nil && strings.Contains(err.Error(), "re_test_key") {
		t.Error("the error carries the API key")
	}
}

// One address per message, and nothing that could turn into more headers.
func TestOnlyOneAddressIsSentTo(t *testing.T) {
	f := &resend{}
	s := sender(f.start(t))
	for _, to := range []string{"", "eve@example.com, dan@example.com", "eve@example.com\r\nBcc: dan@example.com", "Eve <eve@example.com>", "not an address"} {
		if err := s.Send(context.Background(), mail.Message{To: to, Subject: "Hi", Text: "Hi"}); err == nil {
			t.Errorf("sent to %q", to)
		}
	}
	if err := s.Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi\r\nBcc: dan@example.com", Text: "Hi"}); err == nil {
		t.Error("sent a subject with a line break")
	}
	if f.got.path != "" {
		t.Error("Resend was called for a message that shouldn't have gone")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
