// SPDX-License-Identifier: AGPL-3.0-only

package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"time"
)

// Security is how a mail server is reached.
type Security int

const (
	// StartTLS connects plainly and turns TLS on before anything else is
	// said (usually port 587). It's the default, and a server that won't is
	// refused.
	StartTLS Security = iota
	// ImplicitTLS is TLS from the first byte (usually port 465).
	ImplicitTLS
	// NoTLS is for a mail server on the same machine or a private network
	// (Mailpit, a local Postfix), and never with a password.
	NoTLS
)

// SMTP sends through any mail server, from From, with replies to ReplyTo:
// what a self-hosted GuildNet uses where guildnet.chat uses Resend. Use it
// by pointer: it keeps the keys of messages sent in the last day, so a
// retry isn't sent twice.
type SMTP struct {
	Addr               string // host:port, e.g. smtp.example.com:587
	Username, Password string // "" means the server takes mail without signing in
	Security           Security
	From               string // "GuildNet <hello@example.com>"
	ReplyTo            string // "" means none
	// TLSConfig, if set, is how the server's certificate is checked (tests);
	// nil means the system's roots, for Addr's host.
	TLSConfig *tls.Config
	Timeout   time.Duration    // for the whole message; 0 means 30 seconds
	Now       func() time.Time // nil means time.Now

	mu   sync.Mutex
	sent map[string]time.Time // message keys, and when
}

// Send sends m, or says why the server wouldn't.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	if err := check(m); err != nil {
		return err
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("smtp: the From address: %w", err)
	}
	if s.Security == NoTLS && (s.Username != "" || s.Password != "") {
		return errors.New("smtp: a password only goes over TLS")
	}
	if m.Key != "" && !s.claim(m.Key) {
		return nil
	}
	if err := s.send(ctx, from, m); err != nil {
		if m.Key != "" {
			s.release(m.Key)
		}
		return fmt.Errorf("smtp: %w", err)
	}
	return nil
}

func (s *SMTP) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// claim says whether a message with key is to be sent: not if one was in the
// last day.
func (s *SMTP) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, at := range s.sent {
		if now.Sub(at) >= 24*time.Hour {
			delete(s.sent, k)
		}
	}
	if _, ok := s.sent[key]; ok {
		return false
	}
	if s.sent == nil {
		s.sent = map[string]time.Time{}
	}
	s.sent[key] = now
	return true
}

// release lets a message that didn't go be tried again.
func (s *SMTP) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sent, key)
}

func (s *SMTP) send(ctx context.Context, from *mail.Address, m Message) error {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("the server's address %q: %w", s.Addr, err)
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	conf := s.TLSConfig
	if conf == nil {
		conf = &tls.Config{ServerName: host}
	}
	if s.Security == ImplicitTLS {
		tc := tls.Client(conn, conf)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("TLS with %s: %w", s.Addr, err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if s.Security == StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s doesn't offer TLS (STARTTLS)", s.Addr)
		}
		if err := c.StartTLS(conf); err != nil {
			return fmt.Errorf("TLS with %s: %w", s.Addr, err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return fmt.Errorf("signing in: %w", err)
		}
	}
	body, err := s.compose(from, m)
	if err != nil {
		return err
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// compose writes m as a mail message: the text, and the HTML copy beside it
// when there is one.
func (s *SMTP) compose(from *mail.Address, m Message) ([]byte, error) {
	var b bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&b, "%s: %s\r\n", name, value) }
	header("From", from.String())
	header("To", (&mail.Address{Address: m.To}).String())
	if s.ReplyTo != "" {
		r, err := mail.ParseAddress(s.ReplyTo)
		if err != nil {
			return nil, fmt.Errorf("the Reply-To address: %w", err)
		}
		header("Reply-To", r.String())
	}
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", s.now().Format(time.RFC1123Z))
	header("Message-ID", "<"+randomID()+"@"+domainOf(from.Address)+">")
	header("MIME-Version", "1.0")
	if m.HTML == "" {
		header("Content-Type", "text/plain; charset=utf-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	mw := multipart.NewWriter(&b)
	header("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	for _, p := range []struct{ kind, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p.kind + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(w, p.body); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeQP(w interface{ Write([]byte) (int, error) }, s string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(s)); err != nil {
		return err
	}
	return qp.Close()
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func domainOf(address string) string {
	if at := strings.LastIndex(address, "@"); at >= 0 {
		return address[at+1:]
	}
	return "localhost"
}
