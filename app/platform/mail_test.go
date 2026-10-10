// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/mail"
)

func getenvFrom(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// Email goes through Resend or any mail server, whichever is set up, from
// GuildNet's address; with neither, there's no email and everything is done
// by hand.
func TestEmailGoesThroughWhicheverIsSetUp(t *testing.T) {
	from := map[string]string{"GUILDNET_MAIL_FROM": "GuildNet <hello@example.com>", "GUILDNET_MAIL_REPLY_TO": "team@example.com"}
	with := func(more map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range from {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}

	if s, err := newMailer(getenvFrom(nil)); s != nil || err != nil {
		t.Errorf("nothing set up: %v, %v; want no email", s, err)
	}

	s, err := newMailer(getenvFrom(with(map[string]string{"GUILDNET_RESEND_KEY": "re_x"})))
	if r, ok := s.(mail.Resend); err != nil || !ok || r.From != "GuildNet <hello@example.com>" || r.ReplyTo != "team@example.com" {
		t.Errorf("Resend: %#v, %v", s, err)
	}

	s, err = newMailer(getenvFrom(with(map[string]string{"GUILDNET_SMTP_ADDR": "smtp.example.com:587", "GUILDNET_SMTP_USERNAME": "u", "GUILDNET_SMTP_PASSWORD": "p"})))
	if m, ok := s.(*mail.SMTP); err != nil || !ok || m.Addr != "smtp.example.com:587" || m.Security != mail.StartTLS || m.Username != "u" || m.From != "GuildNet <hello@example.com>" {
		t.Errorf("SMTP: %#v, %v", s, err)
	}

	for security, want := range map[string]mail.Security{"starttls": mail.StartTLS, "tls": mail.ImplicitTLS, "none": mail.NoTLS} {
		s, err := newMailer(getenvFrom(with(map[string]string{"GUILDNET_SMTP_ADDR": "mailpit:1025", "GUILDNET_SMTP_SECURITY": security})))
		if m, ok := s.(*mail.SMTP); err != nil || !ok || m.Security != want {
			t.Errorf("GUILDNET_SMTP_SECURITY=%s: %#v, %v", security, s, err)
		}
	}
}

// A setup that can't work is refused at start, saying what's wrong, never
// what the secrets are.
func TestAnEmailSetupThatCantWorkIsRefused(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"no From":           {"GUILDNET_RESEND_KEY": "re_secret", "GUILDNET_MAIL_REPLY_TO": "team@example.com"},
		"both":              {"GUILDNET_RESEND_KEY": "re_secret", "GUILDNET_SMTP_ADDR": "smtp.example.com:587", "GUILDNET_MAIL_FROM": "a@example.com", "GUILDNET_MAIL_REPLY_TO": "b@example.com"},
		"no port":           {"GUILDNET_SMTP_ADDR": "smtp.example.com", "GUILDNET_MAIL_FROM": "a@example.com"},
		"unknown security":  {"GUILDNET_SMTP_ADDR": "smtp.example.com:25", "GUILDNET_SMTP_SECURITY": "maybe", "GUILDNET_MAIL_FROM": "a@example.com"},
		"password, no TLS":  {"GUILDNET_SMTP_ADDR": "smtp.example.com:25", "GUILDNET_SMTP_SECURITY": "none", "GUILDNET_SMTP_PASSWORD": "re_secret", "GUILDNET_MAIL_FROM": "a@example.com"},
		"From isn't a mail": {"GUILDNET_SMTP_ADDR": "smtp.example.com:587", "GUILDNET_MAIL_FROM": "GuildNet"},
	} {
		s, err := newMailer(getenvFrom(vars))
		if err == nil || s != nil {
			t.Errorf("%s: %#v, %v; want refused", name, s, err)
			continue
		}
		if strings.Contains(err.Error(), "re_secret") {
			t.Errorf("%s: the error carries a secret: %v", name, err)
		}
	}
}
