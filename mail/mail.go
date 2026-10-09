// SPDX-License-Identifier: AGPL-3.0-only

// Package mail sends GuildNet's own email (beta invites, invitations into
// networks) from GuildNet's address: plain text, and an HTML copy when there
// is one. It goes through Resend's API (guildnet.chat) or any mail server
// over SMTP (smtp.go).
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// Message is one email to one person.
type Message struct {
	To      string
	Subject string
	Text    string
	// HTML, if set, is the same email for mail apps that show HTML; Text
	// is still sent, for those that don't.
	HTML string
	// Key, if set, makes sending the same message again within a day (a
	// retry) send it only once.
	Key string
}

// Sender sends email.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Resend sends through Resend (resend.com), from From, with replies to
// ReplyTo.
type Resend struct {
	Key     string // the API key: sending access, for From's domain only
	From    string // "GuildNet <hello@mail.guildnet.chat>"
	ReplyTo string
	URL     string       // Resend's API; "" means https://api.resend.com
	HTTP    *http.Client // nil means one with a short timeout
}

// Send sends m, or says why Resend wouldn't.
func (s Resend) Send(ctx context.Context, m Message) error {
	if err := check(m); err != nil {
		return err
	}
	fields := map[string]any{"from": s.From, "to": []string{m.To}, "reply_to": s.ReplyTo, "subject": m.Subject, "text": m.Text}
	if m.HTML != "" {
		fields["html"] = m.HTML
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	base := s.URL
	if base == "" {
		base = "https://api.resend.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	if m.Key != "" {
		req.Header.Set("Idempotency-Key", m.Key)
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var refusal struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(answer, &refusal) != nil || refusal.Message == "" {
		refusal.Message = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("resend: %d %s", resp.StatusCode, oneLine(refusal.Message))
}

// check refuses anything but a plain message to one plain address.
func check(m Message) error {
	a, err := mail.ParseAddress(m.To)
	if err != nil || a.Name != "" || a.Address != m.To {
		return errors.New("mail: send to one plain address")
	}
	if m.Subject == "" || strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("mail: a subject is one line")
	}
	return nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
