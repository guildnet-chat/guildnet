// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/internal/chat"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Multiline (IRCv3 draft/multiline) lets a message span several lines.
// Clients that support it send long messages as one batch and show a batch
// as one message; for everyone else, the server delivers the lines one by
// one, so sending a batch never leaves anyone out.

// onMultiline turns a multiline batch into one PRIVMSG for handle: its lines
// joined by line breaks, or directly where a line continues the one before.
// Other batches are left to other callbacks.
func onMultiline(handle func(ircmsg.Message)) func(*ircevent.Batch) bool {
	return func(b *ircevent.Batch) bool {
		if b.Command != "BATCH" || len(b.Params) < 3 || b.Params[1] != "draft/multiline" || len(b.Items) == 0 {
			return false
		}
		first := b.Items[0].Message
		if first.Command != "PRIVMSG" {
			return false // NOTICEs aren't requests; let them through line by line
		}
		var text strings.Builder
		for i, item := range b.Items {
			if len(item.Params) < 2 {
				continue
			}
			if joined, _ := item.GetTag("draft/multiline-concat"); i > 0 && !joined {
				text.WriteByte('\n')
			}
			text.WriteString(item.Params[1])
		}
		msg := first
		msg.Params = []string{b.Params[2], text.String()}
		// The whole message's id, and who sent it, are on the BATCH line, not
		// its lines.
		for _, tag := range []string{"msgid", "account"} {
			if ok, v := b.GetTag(tag); ok {
				msg.SetTag(tag, v)
			}
		}
		handle(msg)
		return true
	}
}

// multilineLimits says whether conn can send multiline messages, and how
// big one may be.
func multilineLimits(conn *ircevent.Connection) (ok bool, maxBytes, maxLines int) {
	v, ok := conn.AcknowledgedCaps()["draft/multiline"]
	if !ok {
		return false, 0, 0
	}
	if _, batch := conn.AcknowledgedCaps()["batch"]; !batch {
		return false, 0, 0
	}
	maxBytes, maxLines = 4096, 100
	for _, kv := range strings.Split(v, ",") {
		k, val, _ := strings.Cut(kv, "=")
		n, err := strconv.Atoi(val)
		if err != nil || n <= 0 {
			continue
		}
		switch k {
		case "max-bytes":
			maxBytes = n
		case "max-lines":
			maxLines = n
		}
	}
	return true, maxBytes, maxLines
}

// mlLine is a line of a multiline message.
type mlLine struct {
	text   string
	concat bool // carries on the line before it, with no line break
}

// sendMultiline sends lines as one multiline message to target. tags (a
// reply's, say) are the whole message's, so they go on the BATCH line.
func sendMultiline(conn *ircevent.Connection, target string, lines []mlLine, tags map[string]string) error {
	ref := "gn" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := conn.SendWithTags(tags, "BATCH", "+"+ref, "draft/multiline", target); err != nil {
		return err
	}
	for _, l := range lines {
		tags := map[string]string{"batch": ref}
		if l.concat {
			tags["draft/multiline-concat"] = ""
		}
		if err := conn.SendWithTags(tags, "PRIVMSG", target, l.text); err != nil {
			return err
		}
	}
	return conn.Send("BATCH", "-"+ref)
}

// multilineText is what a multiline message says.
func multilineText(lines []mlLine) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 && !l.concat {
			b.WriteByte('\n')
		}
		b.WriteString(l.text)
	}
	return b.String()
}

// split cuts text into pieces of at most max bytes that add back up to text
// exactly: after a space where possible, so a wrapped line reads the same
// once rejoined.
func split(text string, max int) []string {
	var out []string
	for len(text) > max {
		cut := strings.LastIndexByte(text[:max], ' ') + 1
		if cut <= 0 {
			cut = max
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}

// sendLines sends a reply's lines as multiline messages, as few as the
// server's limits allow, and returns what was sent, one chat line per
// message.
func (r *Runner) sendLines(conn *ircevent.Connection, target string, texts []string, maxBytes, maxLines int, first *firstTags) []chat.Line {
	var sent []chat.Line
	var batch []mlLine
	size := 0
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		if err := sendMultiline(conn, target, batch, first.take()); err != nil {
			monitor.Problem(r.logger, "send to %s: %v", target, err)
			return false
		}
		sent = append(sent, chat.Line{Text: multilineText(batch)})
		batch, size = nil, 0
		return true
	}
	for _, text := range texts {
		pieces := split(text, maxLineBytes)
		if len(batch)+len(pieces) > maxLines || size+len(text)+1 > maxBytes {
			if !flush() {
				return sent
			}
		}
		for i, p := range pieces {
			batch = append(batch, mlLine{text: p, concat: i > 0})
		}
		size += len(text) + 1
	}
	flush()
	return sent
}

// firstTags are tags for the first message of a reply only: the reply
// (+draft/reply) is to the question once, however many messages it takes.
type firstTags struct{ tags map[string]string }

func (f *firstTags) take() map[string]string {
	if f == nil {
		return nil
	}
	t := f.tags
	f.tags = nil
	return t
}

// say sends one line, as a /me action or not, with tags if there are any.
func say(conn *ircevent.Connection, target, text string, action bool, tags map[string]string) error {
	if action {
		text = "\x01ACTION " + text + "\x01"
	}
	if len(tags) == 0 {
		return conn.Privmsg(target, text)
	}
	return conn.SendWithTags(tags, "PRIVMSG", target, text)
}

// hasCap says whether the server acknowledged cap for conn.
func hasCap(conn *ircevent.Connection, cap string) bool {
	_, ok := conn.AcknowledgedCaps()[cap]
	return ok
}
