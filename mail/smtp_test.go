// SPDX-License-Identifier: AGPL-3.0-only

package mail_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
)

// smtpServer is a stand-in for a mail server (Postfix, Mailpit, a provider's
// relay): it speaks enough SMTP to take a message, and keeps what it got.
type smtpServer struct {
	ln       net.Listener
	cert     tls.Certificate
	roots    *x509.CertPool
	implicit bool // TLS from the first byte (port 465), not STARTTLS
	noTLS    bool // offers no STARTTLS

	mu     sync.Mutex
	refuse string // a reply to RCPT TO, e.g. "550 5.1.1 No such user"
	seen   smtpSeen
}

// smtpSeen is what the server has been sent.
type smtpSeen struct {
	sessions int
	auth     string // the decoded AUTH PLAIN, "" if none
	tls      bool   // the last message came over TLS
	from, to string
	data     string
}

func newSMTPServer(t *testing.T, tweak func(*smtpServer)) *smtpServer {
	t.Helper()
	// httptest's certificate (for 127.0.0.1 and example.com) will do.
	h := httptest.NewTLSServer(nil)
	cert := h.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(h.Certificate())
	h.Close()
	s := &smtpServer{cert: cert, roots: roots}
	if tweak != nil {
		tweak(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if s.implicit {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpServer) addr() string { return s.ln.Addr().String() }

func (s *smtpServer) got() smtpSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

func (s *smtpServer) refuseWith(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = reply
}

func (s *smtpServer) note(f func(*smtpSeen)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.seen)
}

func (s *smtpServer) serve(c net.Conn) {
	defer func() { c.Close() }()
	s.note(func(seen *smtpSeen) { seen.sessions++ })
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(line string) { w.WriteString(line + "\r\n"); w.Flush() }
	say("220 mail.example.com ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		_, secure := c.(*tls.Conn)
		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			w.WriteString("250-mail.example.com\r\n")
			if !secure && !s.noTLS {
				w.WriteString("250-STARTTLS\r\n")
			}
			say("250 AUTH PLAIN")
		case upper == "STARTTLS":
			say("220 Go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if tc.Handshake() != nil {
				return
			}
			c = tc
			r, w = bufio.NewReader(c), bufio.NewWriter(c)
		case strings.HasPrefix(upper, "AUTH "):
			parts := strings.Fields(line)
			b, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			s.note(func(seen *smtpSeen) { seen.auth = string(b) })
			say("235 2.7.0 Authenticated")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.note(func(seen *smtpSeen) { seen.from = line[len("MAIL FROM:"):] })
			say("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			s.mu.Lock()
			refuse := s.refuse
			s.mu.Unlock()
			if refuse != "" {
				say(refuse)
				continue
			}
			s.note(func(seen *smtpSeen) { seen.to = line[len("RCPT TO:"):] })
			say("250 OK")
		case upper == "DATA":
			say("354 End data with <CR><LF>.<CR><LF>")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			s.note(func(seen *smtpSeen) { seen.data, seen.tls = b.String(), secure })
			say("250 OK: queued")
		case upper == "RSET" || upper == "NOOP":
			say("250 OK")
		case upper == "QUIT":
			say("221 Bye")
			return
		default:
			say("502 Not implemented")
		}
	}
}

// message is what the server took, read as a mail app would: its headers,
// and its bodies by type, decoded.
func (s *smtpServer) message(t *testing.T) (netmail.Header, map[string]string) {
	t.Helper()
	data := s.got().data
	if data == "" {
		t.Fatal("no message arrived")
	}
	m, err := netmail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("reading the message: %v\n%s", err, data)
	}
	body, _ := io.ReadAll(m.Body)
	media, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type: %v", err)
	}
	bodies := map[string]string{}
	if !strings.HasPrefix(media, "multipart/") {
		bodies[media] = decode(t, m.Header.Get("Content-Transfer-Encoding"), string(body))
		return m.Header, bodies
	}
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p)
		pm, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		bodies[pm] = decode(t, p.Header.Get("Content-Transfer-Encoding"), string(b))
	}
	return m.Header, bodies
}

func decode(t *testing.T, encoding, s string) string {
	t.Helper()
	if strings.EqualFold(encoding, "quoted-printable") {
		b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
		if err != nil {
			t.Fatalf("quoted-printable: %v", err)
		}
		s = string(b)
	}
	// SMTP ends a message with a line break, whatever it is.
	return strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

func smtpSender(s *smtpServer, tweak func(*mail.SMTP)) *mail.SMTP {
	host, _, _ := net.SplitHostPort(s.addr())
	m := &mail.SMTP{
		Addr: s.addr(), Username: "postmaster@example.com", Password: "smtp-secret",
		From: "GuildNet <hello@mail.example.com>", ReplyTo: "hello@example.com",
		TLSConfig: &tls.Config{RootCAs: s.roots, ServerName: host},
	}
	if tweak != nil {
		tweak(m)
	}
	return m
}

// A self-hosted GuildNet sends its email through any mail server: over TLS
// (STARTTLS), signed in, from its address with replies to its inbox.
func TestAnEmailIsSentThroughAMailServer(t *testing.T) {
	srv := newSMTPServer(t, nil)

	err := smtpSender(srv, nil).Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "You're invited to Acme’s network", Text: "Hi,\n\nYou're invited."})

	if err != nil {
		t.Fatal(err)
	}
	seen := srv.got()
	if !seen.tls {
		t.Error("sent without TLS")
	}
	if seen.auth != "\x00postmaster@example.com\x00smtp-secret" {
		t.Errorf("signed in as %q", seen.auth)
	}
	if seen.from != "<hello@mail.example.com>" || seen.to != "<eve@example.com>" {
		t.Errorf("envelope from %s to %s", seen.from, seen.to)
	}
	h, bodies := srv.message(t)
	for name, want := range map[string]netmail.Address{
		"From":     {Name: "GuildNet", Address: "hello@mail.example.com"},
		"To":       {Address: "eve@example.com"},
		"Reply-To": {Address: "hello@example.com"},
	} {
		if a, err := netmail.ParseAddress(h.Get(name)); err != nil || *a != want {
			t.Errorf("%s: %q, want %v", name, h.Get(name), want.String())
		}
	}
	if subject, _ := new(mime.WordDecoder).DecodeHeader(h.Get("Subject")); subject != "You're invited to Acme’s network" {
		t.Errorf("Subject %q", subject)
	}
	if h.Get("Date") == "" || h.Get("Message-ID") == "" {
		t.Error("no Date or Message-ID")
	}
	if bodies["text/plain"] != "Hi,\n\nYou're invited." || len(bodies) != 1 {
		t.Errorf("bodies %q, want just the text", bodies)
	}
}

// With an HTML copy, mail apps get both and show whichever they can.
func TestAnHTMLCopyGoesWithTheTextOverSMTP(t *testing.T) {
	srv := newSMTPServer(t, nil)

	if err := smtpSender(srv, nil).Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi there", HTML: "<p>Hi <b>there</b></p>"}); err != nil {
		t.Fatal(err)
	}

	if _, bodies := srv.message(t); bodies["text/plain"] != "Hi there" || bodies["text/html"] != "<p>Hi <b>there</b></p>" {
		t.Errorf("bodies %q, want the text and the HTML", bodies)
	}
}

// Port 465's kind of TLS, from the first byte, works too.
func TestAMailServerCanBeReachedOverImplicitTLS(t *testing.T) {
	srv := newSMTPServer(t, func(s *smtpServer) { s.implicit = true })

	err := smtpSender(srv, func(m *mail.SMTP) { m.Security = mail.ImplicitTLS }).Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi"})

	if err != nil {
		t.Fatal(err)
	}
	if seen := srv.got(); !seen.tls || seen.data == "" {
		t.Error("not sent over TLS")
	}
}

// A server that won't do TLS never gets the password or the message.
func TestThePasswordNeverGoesOutWithoutTLS(t *testing.T) {
	srv := newSMTPServer(t, func(s *smtpServer) { s.noTLS = true })

	err := smtpSender(srv, nil).Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi"})

	if err == nil {
		t.Fatal("sent without TLS")
	}
	if seen := srv.got(); seen.auth != "" || seen.data != "" {
		t.Errorf("the server got auth %q and data %q", seen.auth, seen.data)
	}
}

// A mail server on the same machine or a private network (Mailpit, a local
// Postfix) can be used without TLS when that's asked for, and then only
// without a password.
func TestALocalMailServerCanBeUsedWithoutTLS(t *testing.T) {
	srv := newSMTPServer(t, func(s *smtpServer) { s.noTLS = true })

	err := smtpSender(srv, func(m *mail.SMTP) { m.Security, m.Username, m.Password = mail.NoTLS, "", "" }).Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.got().data == "" {
		t.Error("nothing arrived")
	}

	err = smtpSender(srv, func(m *mail.SMTP) { m.Security = mail.NoTLS }).Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi"})
	if seen := srv.got(); err == nil || seen.auth != "" {
		t.Errorf("a password went without TLS (err %v, auth %q)", err, seen.auth)
	}
}

// A retry of the same message (the same key) within a day sends it once.
func TestTheSameMessageIsSentOnceADay(t *testing.T) {
	srv := newSMTPServer(t, nil)
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	s := smtpSender(srv, func(m *mail.SMTP) { m.Now = func() time.Time { return now } })
	m := mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi", Key: "beta-invite/eve"}

	for range 2 {
		if err := s.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.got().sessions; n != 1 {
		t.Errorf("%d sessions for one message sent twice, want 1", n)
	}

	now = now.Add(25 * time.Hour)
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if n := srv.got().sessions; n != 2 {
		t.Errorf("a day later, %d sessions, want 2", n)
	}
}

// A message the server refuses can be tried again, and the refusal says why
// without the password.
func TestARefusedMessageSaysWhyAndCanBeRetried(t *testing.T) {
	srv := newSMTPServer(t, nil)
	srv.refuseWith("550 5.1.1 No such user here")
	s := smtpSender(srv, nil)
	m := mail.Message{To: "eve@example.com", Subject: "Hi", Text: "Hi", Key: "k"}

	err := s.Send(context.Background(), m)

	if err == nil || !strings.Contains(err.Error(), "550") || !strings.Contains(err.Error(), "No such user") {
		t.Errorf("err = %v, want the server's reason", err)
	}
	if err != nil && strings.Contains(err.Error(), "smtp-secret") {
		t.Error("the error carries the password")
	}
	srv.refuseWith("")
	if err := s.Send(context.Background(), m); err != nil || srv.got().data == "" {
		t.Errorf("the retry: %v", err)
	}
}

// One plain address per message, and no line breaks that could become more
// headers: refused before anything is sent.
func TestOnlyOneAddressIsSentToOverSMTP(t *testing.T) {
	srv := newSMTPServer(t, nil)
	s := smtpSender(srv, nil)
	for _, to := range []string{"", "eve@example.com, dan@example.com", "eve@example.com\r\nBcc: dan@example.com", "Eve <eve@example.com>", "not an address"} {
		if err := s.Send(context.Background(), mail.Message{To: to, Subject: "Hi", Text: "Hi"}); err == nil {
			t.Errorf("sent to %q", to)
		}
	}
	if err := s.Send(context.Background(), mail.Message{To: "eve@example.com", Subject: "Hi\r\nBcc: dan@example.com", Text: "Hi"}); err == nil {
		t.Error("sent a subject with a line break")
	}
	if n := srv.got().sessions; n != 0 {
		t.Error("the server was reached for a message that shouldn't have gone")
	}
}
