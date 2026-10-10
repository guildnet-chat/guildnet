// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"fmt"
	"log"
	"net"
	netmail "net/mail"
	"os"
	"strings"

	"github.com/guildnet-chat/guildnet/mail"
)

// mailer is how GuildNet's own email goes, from what's set up; nil means no
// email (everything is done by hand). A setup that can't work stops it.
func mailer(logger *log.Logger) mail.Sender {
	s, err := newMailer(os.Getenv)
	if err != nil {
		logger.Fatalf("email: %v", err)
	}
	switch m := s.(type) {
	case mail.Resend:
		logger.Printf("email: through Resend, from %s, replies to %s", m.From, m.ReplyTo)
	case *mail.SMTP:
		logger.Printf("email: through %s, from %s", m.Addr, m.From)
	}
	return s
}

// newMailer reads email's setup: Resend (GUILDNET_RESEND_KEY) or a mail
// server (GUILDNET_SMTP_ADDR, host:port; GUILDNET_SMTP_USERNAME and
// _PASSWORD; GUILDNET_SMTP_SECURITY starttls, the default, tls or none),
// from GUILDNET_MAIL_FROM with replies to GUILDNET_MAIL_REPLY_TO.
func newMailer(getenv func(string) string) (mail.Sender, error) {
	key, addr := getenv("GUILDNET_RESEND_KEY"), getenv("GUILDNET_SMTP_ADDR")
	from, replyTo := getenv("GUILDNET_MAIL_FROM"), getenv("GUILDNET_MAIL_REPLY_TO")
	switch {
	case key == "" && addr == "":
		return nil, nil
	case key != "" && addr != "":
		return nil, errors.New("set GUILDNET_RESEND_KEY or GUILDNET_SMTP_ADDR, not both")
	case from == "":
		return nil, errors.New("GUILDNET_MAIL_FROM says who email is from")
	}
	if _, err := netmail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("GUILDNET_MAIL_FROM: %v", err)
	}
	if key != "" {
		if replyTo == "" {
			return nil, errors.New("GUILDNET_MAIL_REPLY_TO says where replies go")
		}
		return mail.Resend{Key: key, From: from, ReplyTo: replyTo}, nil
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("GUILDNET_SMTP_ADDR is host:port, e.g. smtp.example.com:587")
	}
	m := &mail.SMTP{Addr: addr, Username: getenv("GUILDNET_SMTP_USERNAME"), Password: getenv("GUILDNET_SMTP_PASSWORD"), From: from, ReplyTo: replyTo}
	switch strings.ToLower(getenv("GUILDNET_SMTP_SECURITY")) {
	case "", "starttls":
		m.Security = mail.StartTLS
	case "tls":
		m.Security = mail.ImplicitTLS
	case "none":
		m.Security = mail.NoTLS
		if m.Username != "" || m.Password != "" {
			return nil, errors.New("GUILDNET_SMTP_SECURITY=none is for a mail server on this machine or a private network, without a password")
		}
	default:
		return nil, errors.New("GUILDNET_SMTP_SECURITY is starttls (the default), tls or none")
	}
	return m, nil
}
