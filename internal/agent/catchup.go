// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/chat"
)

// ChannelLog is what was said in one channel during a catch-up window.
type ChannelLog struct {
	Name  string
	Topic string
	Lines []chat.Line
}

// CatchupRequest asks for a summary of what someone missed.
type CatchupRequest struct {
	Model    string
	For      string // the person's nick
	Since    time.Time
	Location *time.Location // the company timezone
	Channels []ChannelLog
}

// CatchupPrompt asks for a short IRC-ready summary of what req.For missed.
func CatchupPrompt(req CatchupRequest) string {
	var s strings.Builder
	fmt.Fprintf(&s, "%s was away from GuildNet's IRC network from %s until now. Here's what was said in their channels:\n",
		req.For, req.Since.In(orUTC(req.Location)).Format("Mon 15:04 MST"))
	for _, ch := range req.Channels {
		fmt.Fprintf(&s, "\n<channel name=%q", ch.Name)
		if ch.Topic != "" {
			fmt.Fprintf(&s, " topic=%q", ch.Topic)
		}
		s.WriteString(">\n")
		for _, l := range ch.Lines {
			s.WriteString(l.In(req.Location))
			s.WriteByte('\n')
		}
		s.WriteString("</channel>\n")
	}
	fmt.Fprintf(&s, `
Write %[1]s a catch-up they can read in 20 seconds, as plain IRC lines (no markdown, no headings, no bold):
1. First line starts "For you:" and lists anything that mentions %[1]s, asks them something, or waits on them, with who and where. If nothing does, write "For you: nothing waiting on you."
2. If decisions were made, one line starting "Decided:".
3. Then one short line per channel where something happened, starting with the channel name. Skip channels with only chatter.
At most 8 lines. Be concrete: names, numbers, dates. Don't pad.`, req.For)
	return s.String()
}

// orUTC is loc, or UTC when it's unset.
func orUTC(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}
