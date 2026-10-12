// SPDX-License-Identifier: AGPL-3.0-only

// Package chat keeps recent conversation and channel topics in memory.
package chat

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Line is one message as the agents see it.
type Line struct {
	At     time.Time
	Nick   string
	Text   string
	Action bool   // a /me line
	MsgID  string // the server's id for it, if it gave one (IRCv3 msgid)
}

// String formats the line like an IRC client log, in UTC.
func (l Line) String() string { return l.In(time.UTC) }

// In formats the line like an IRC client log, with its time in loc. A
// message of several lines (multiline) shows each as its sender's, so none
// of them can pass as someone else's.
func (l Line) In(loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	ts := l.At.In(loc).Format("15:04")
	prefix := fmt.Sprintf("[%s] <%s> ", ts, l.Nick)
	if l.Action {
		prefix = fmt.Sprintf("[%s] * %s ", ts, l.Nick)
	}
	lines := strings.Split(strings.ReplaceAll(l.Text, "\r\n", "\n"), "\n")
	for i, s := range lines {
		lines[i] = prefix + strings.TrimSuffix(s, "\r")
	}
	return strings.Join(lines, "\n")
}

// Log holds the last max lines per conversation key (an archive for
// catch-ups; agents see only the most recent few) and each channel's topic.
// Keys are channel names or DMKey values, compared case-insensitively.
type Log struct {
	mu     sync.Mutex
	max    int
	lines  map[string][]Line
	topics map[string]string
}

// NewLog returns a Log keeping max lines per conversation.
func NewLog(max int) *Log {
	return &Log{max: max, lines: map[string][]Line{}, topics: map[string]string{}}
}

// DMKey is the conversation key for a DM between an agent and someone else.
func DMKey(agent, peer string) string {
	return "dm:" + strings.ToLower(agent) + ":" + strings.ToLower(peer)
}

// Add appends a line to the conversation at key.
func (g *Log) Add(key string, line Line) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := strings.ToLower(key)
	ls := append(g.lines[k], line)
	if len(ls) > g.max {
		ls = ls[len(ls)-g.max:]
	}
	g.lines[k] = ls
}

// Merge adds earlier lines, such as history replayed from the server, into
// the conversation at key. Lines already present are skipped and the result
// stays in time order, so replaying twice is harmless.
func (g *Log) Merge(key string, lines []Line) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := strings.ToLower(key)
	have := map[Line]bool{}
	for _, l := range g.lines[k] {
		have[l] = true
	}
	merged := append([]Line(nil), g.lines[k]...)
	for _, l := range lines {
		if !have[l] && !g.hasSame(g.lines[k], l) {
			merged = append(merged, l)
			have[l] = true
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].At.Before(merged[j].At) })
	if len(merged) > g.max {
		merged = merged[len(merged)-g.max:]
	}
	g.lines[k] = merged
}

// hasSame reports whether ls holds a line with the same sender and text
// within a few seconds of l: the same message seen live and in history can
// carry slightly different timestamps.
func (g *Log) hasSame(ls []Line, l Line) bool {
	for _, x := range ls {
		d := x.At.Sub(l.At)
		if x.Nick == l.Nick && x.Text == l.Text && x.Action == l.Action && d < 5*time.Second && d > -5*time.Second {
			return true
		}
	}
	return false
}

// Recent returns up to the last n lines of the conversation at key, oldest
// first.
func (g *Log) Recent(key string, n int) []Line {
	g.mu.Lock()
	defer g.mu.Unlock()
	ls := g.lines[strings.ToLower(key)]
	if n > 0 && len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return append([]Line(nil), ls...)
}

// Since returns the lines at key said after t, oldest first.
func (g *Log) Since(key string, t time.Time) []Line {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Line
	for _, l := range g.lines[strings.ToLower(key)] {
		if l.At.After(t) {
			out = append(out, l)
		}
	}
	return out
}

// SetTopic records a channel's topic.
func (g *Log) SetTopic(channel, topic string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.topics[strings.ToLower(channel)] = topic
}

// Topic returns a channel's topic, or "".
func (g *Log) Topic(channel string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.topics[strings.ToLower(channel)]
}
