// SPDX-License-Identifier: AGPL-3.0-only

// Package ircfake is a tiny in-process IRC server for behaviour tests. It
// speaks just enough IRC for irc-go clients: registration, CAP, SASL PLAIN
// logins, account-tag, JOIN, PART, TOPIC, PRIVMSG/NOTICE to channels and
// nicks, PING, and QUIT.
package ircfake

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ergochat/irc-go/ircmsg"
)

const serverName = "irc.fake"

// supportedCaps are the IRCv3 capabilities the fake offers.
var supportedCaps = []string{"sasl", "account-tag", "message-tags", "server-time", "batch", "draft/chathistory", "away-notify", "draft/multiline"}

// capValues are the values advertised with some caps, as Ergo does.
var capValues = map[string]string{"draft/multiline": "max-bytes=4096,max-lines=100"}

// Server is a running fake IRC server.
type Server struct {
	ln       net.Listener
	mu       sync.Mutex
	nicks    map[string]*client // by lowercase nick
	channels map[string]*channel
	clients  map[*client]bool
	accounts map[string]string // account -> password
}

type channel struct {
	name    string
	topic   string
	members map[*client]bool
	history []stored // messages, oldest first
}

type stored struct {
	at   time.Time
	line string // without tags
}

type client struct {
	srv         *Server
	conn        net.Conn
	wmu         sync.Mutex
	nick, user  string
	registered  bool
	negotiating bool // between CAP LS and CAP END, registration waits
	caps        map[string]bool
	account     string              // set once logged in with SASL
	batches     map[string]*inBatch // multiline messages being sent, by ref
}

// inBatch is a multiline message a client is sending.
type inBatch struct {
	target string
	lines  []mlLine
}

type mlLine struct {
	text   string
	concat bool // joins the line before it without a line break
}

// Start listens on a random localhost port.
func Start() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		nicks:    map[string]*client{},
		channels: map[string]*channel{},
		clients:  map[*client]bool{},
		accounts: map[string]string{},
	}
	go s.accept()
	return s, nil
}

// AddAccount registers an account people can log in to with SASL PLAIN.
func (s *Server) AddAccount(name, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[strings.ToLower(name)] = password
}

// Addr is host:port for clients.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the server and drops every client.
func (s *Server) Close() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.conn.Close()
	}
}

// Topic is a channel's topic.
func (s *Server) Topic(ch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.channels[strings.ToLower(ch)]; c != nil {
		return c.topic
	}
	return ""
}

// Members returns the nicks in a channel, sorted.
func (s *Server) Members(ch string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if c := s.channels[strings.ToLower(ch)]; c != nil {
		for m := range c.members {
			out = append(out, m.nick)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &client{srv: s, conn: conn, caps: map[string]bool{}}
		s.mu.Lock()
		s.clients[c] = true
		s.mu.Unlock()
		go c.serve()
	}
}

func (c *client) serve() {
	defer c.quit()
	sc := bufio.NewScanner(c.conn)
	for sc.Scan() {
		msg, err := ircmsg.ParseLine(sc.Text())
		if err != nil {
			continue
		}
		if !c.handle(msg) {
			return
		}
	}
}

func (c *client) handle(m ircmsg.Message) bool {
	s := c.srv
	p := func(i int) string {
		if i < len(m.Params) {
			return m.Params[i]
		}
		return ""
	}
	switch strings.ToUpper(m.Command) {
	case "CAP":
		c.handleCap(p(0), p(1))
	case "AUTHENTICATE":
		c.handleAuthenticate(p(0))
	case "NICK":
		s.mu.Lock()
		if other := s.nicks[strings.ToLower(p(0))]; other != nil && other != c {
			s.mu.Unlock()
			c.send(":%s 433 * %s :Nickname is already in use", serverName, p(0))
			return true
		}
		delete(s.nicks, strings.ToLower(c.nick))
		c.nick = p(0)
		s.nicks[strings.ToLower(c.nick)] = c
		s.mu.Unlock()
		c.maybeWelcome()
	case "USER":
		c.user = p(0)
		c.maybeWelcome()
	case "PING":
		c.send(":%s PONG %s :%s", serverName, serverName, p(0))
	case "JOIN":
		for _, name := range strings.Split(p(0), ",") {
			c.join(name)
		}
	case "PART":
		for _, name := range strings.Split(p(0), ",") {
			s.mu.Lock()
			ch := s.channels[strings.ToLower(name)]
			member := ch != nil && ch.members[c]
			s.mu.Unlock()
			if member {
				s.toChannel(ch, nil, c, fmt.Sprintf(":%s PART %s", c.prefix(), ch.name))
				s.mu.Lock()
				delete(ch.members, c)
				s.mu.Unlock()
			}
		}
	case "TOPIC":
		s.mu.Lock()
		ch := s.channels[strings.ToLower(p(0))]
		if ch != nil && len(m.Params) > 1 {
			ch.topic = p(1)
		}
		s.mu.Unlock()
		if ch != nil && len(m.Params) > 1 {
			s.toChannel(ch, nil, c, fmt.Sprintf(":%s TOPIC %s :%s", c.prefix(), ch.name, p(1)))
		}
	case "BATCH":
		ref := p(0)
		switch {
		case strings.HasPrefix(ref, "+") && p(1) == "draft/multiline":
			if c.batches == nil {
				c.batches = map[string]*inBatch{}
			}
			c.batches[ref[1:]] = &inBatch{target: p(2)}
		case strings.HasPrefix(ref, "-"):
			if b := c.batches[ref[1:]]; b != nil {
				delete(c.batches, ref[1:])
				c.deliverMultiline(b)
			}
		}
	case "PRIVMSG", "NOTICE":
		if ok, ref := m.GetTag("batch"); ok {
			if b := c.batches[ref]; b != nil {
				joined, _ := m.GetTag("draft/multiline-concat")
				b.lines = append(b.lines, mlLine{text: p(1), concat: joined})
				return true
			}
		}
		line := fmt.Sprintf(":%s %s %s :%s", c.prefix(), strings.ToUpper(m.Command), p(0), p(1))
		s.mu.Lock()
		ch := s.channels[strings.ToLower(p(0))]
		to := s.nicks[strings.ToLower(p(0))]
		s.mu.Unlock()
		if ch != nil {
			s.mu.Lock()
			ch.history = append(ch.history, stored{at: time.Now().UTC(), line: line})
			s.mu.Unlock()
			s.toChannel(ch, c, c, line)
		} else if to != nil {
			to.sendFrom(c, line)
		}
	case "AWAY":
		// Tell channel peers who asked for away-notify: "AWAY :reason" when
		// someone leaves, a bare "AWAY" when they're back.
		line := fmt.Sprintf(":%s AWAY", c.prefix())
		if p(0) != "" {
			line += " :" + p(0)
		}
		for _, m := range c.peers() {
			m.wmu.Lock()
			wants := m.caps["away-notify"]
			m.wmu.Unlock()
			if wants {
				m.sendFrom(c, line)
			}
		}
	case "CHATHISTORY":
		// Only LATEST <target> * <limit>, which is what clients use to catch up.
		if !strings.EqualFold(p(0), "LATEST") {
			c.send(":%s FAIL CHATHISTORY INVALID_PARAMS %s :only LATEST is supported", serverName, p(0))
			return true
		}
		limit, _ := strconv.Atoi(p(3))
		s.mu.Lock()
		ch := s.channels[strings.ToLower(p(1))]
		var msgs []stored
		if ch != nil {
			msgs = ch.history
			if limit > 0 && len(msgs) > limit {
				msgs = msgs[len(msgs)-limit:]
			}
			msgs = append([]stored(nil), msgs...)
		}
		s.mu.Unlock()
		id := fmt.Sprintf("h%d", time.Now().UnixNano())
		c.send(":%s BATCH +%s chathistory %s", serverName, id, p(1))
		for _, m := range msgs {
			c.send("@batch=%s;time=%s %s", id, m.at.Format("2006-01-02T15:04:05.000Z"), m.line)
		}
		c.send(":%s BATCH -%s", serverName, id)
	case "QUIT":
		// Tell everyone who shares a channel, once each.
		line := fmt.Sprintf(":%s QUIT :%s", c.prefix(), p(0))
		for _, m := range c.peers() {
			m.sendFrom(c, line)
		}
		return false
	}
	return true
}

func (c *client) handleCap(sub, arg string) {
	switch strings.ToUpper(sub) {
	case "LS":
		c.negotiating = true
		var ls []string
		for _, name := range supportedCaps {
			if v, ok := capValues[name]; ok {
				name += "=" + v
			}
			ls = append(ls, name)
		}
		c.send(":%s CAP * LS :%s", serverName, strings.Join(ls, " "))
	case "REQ":
		requested := strings.Fields(arg)
		for _, want := range requested {
			if !contains(supportedCaps, want) {
				c.send(":%s CAP * NAK :%s", serverName, arg)
				return
			}
		}
		c.wmu.Lock()
		for _, want := range requested {
			c.caps[want] = true
		}
		c.wmu.Unlock()
		c.send(":%s CAP * ACK :%s", serverName, arg)
	case "END":
		c.negotiating = false
		c.maybeWelcome()
	}
}

func (c *client) handleAuthenticate(arg string) {
	if strings.EqualFold(arg, "PLAIN") {
		c.send("AUTHENTICATE +")
		return
	}
	nick := c.nick
	if nick == "" {
		nick = "*"
	}
	raw, err := base64.StdEncoding.DecodeString(arg)
	parts := strings.Split(string(raw), "\x00")
	if err != nil || len(parts) != 3 {
		c.send(":%s 904 %s :SASL authentication failed", serverName, nick)
		return
	}
	account, password := strings.ToLower(parts[1]), parts[2]
	c.srv.mu.Lock()
	want, ok := c.srv.accounts[account]
	c.srv.mu.Unlock()
	if !ok || want != password {
		c.send(":%s 904 %s :SASL authentication failed", serverName, nick)
		return
	}
	c.wmu.Lock()
	c.account = account
	c.wmu.Unlock()
	c.send(":%s 900 %s %s %s :You are now logged in as %s", serverName, nick, nick, account, account)
	c.send(":%s 903 %s :SASL authentication successful", serverName, nick)
}

func (c *client) maybeWelcome() {
	if c.registered || c.negotiating || c.nick == "" || c.user == "" {
		return
	}
	c.registered = true
	c.send(":%s 001 %s :Welcome to the fake network", serverName, c.nick)
	c.send(":%s 005 %s CHANTYPES=# :are supported", serverName, c.nick)
	c.send(":%s 422 %s :No MOTD", serverName, c.nick)
}

func (c *client) join(name string) {
	s := c.srv
	s.mu.Lock()
	ch := s.channels[strings.ToLower(name)]
	if ch == nil {
		ch = &channel{name: name, members: map[*client]bool{}}
		s.channels[strings.ToLower(name)] = ch
	}
	ch.members[c] = true
	// Like Ergo, record joins in history as HistServ lines, which is how
	// clients without event-playback see them.
	ch.history = append(ch.history, stored{at: time.Now().UTC(),
		line: fmt.Sprintf(":HistServ!HistServ@fake PRIVMSG %s :%s joined the channel", ch.name, c.nick)})
	topic := ch.topic
	var names []string
	for m := range ch.members {
		names = append(names, m.nick)
	}
	s.mu.Unlock()

	s.toChannel(ch, nil, c, fmt.Sprintf(":%s JOIN %s", c.prefix(), ch.name))
	if topic != "" {
		c.send(":%s 332 %s %s :%s", serverName, c.nick, ch.name, topic)
	}
	c.send(":%s 353 %s = %s :%s", serverName, c.nick, ch.name, strings.Join(names, " "))
	c.send(":%s 366 %s %s :End of /NAMES list", serverName, c.nick, ch.name)
}

// toChannel sends line from sender to every member except skip.
func (s *Server) toChannel(ch *channel, skip, sender *client, line string) {
	s.mu.Lock()
	var members []*client
	for m := range ch.members {
		if m != skip {
			members = append(members, m)
		}
	}
	s.mu.Unlock()
	for _, m := range members {
		m.sendFrom(sender, line)
	}
}

func (c *client) quit() {
	s := c.srv
	s.mu.Lock()
	delete(s.clients, c)
	if s.nicks[strings.ToLower(c.nick)] == c {
		delete(s.nicks, strings.ToLower(c.nick))
	}
	for _, ch := range s.channels {
		delete(ch.members, c)
	}
	s.mu.Unlock()
	c.conn.Close()
}

// peers returns everyone who shares a channel with c, once each.
func (c *client) peers() []*client {
	s := c.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[*client]bool{}
	var out []*client
	for _, ch := range s.channels {
		if !ch.members[c] {
			continue
		}
		for m := range ch.members {
			if m != c && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func (c *client) prefix() string { return fmt.Sprintf("%s!%s@fake", c.nick, c.user) }

// sendFrom sends a line from sender, tagged with the sender's account when
// this client asked for account-tag and the sender is logged in.
func (c *client) sendFrom(sender *client, line string) {
	account := ""
	if sender != nil {
		sender.wmu.Lock()
		account = sender.account
		sender.wmu.Unlock()
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if account != "" && c.caps["account-tag"] {
		line = "@account=" + account + " " + line
	}
	fmt.Fprintf(c.conn, "%s\r\n", line)
}

func (c *client) send(format string, args ...any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	fmt.Fprintf(c.conn, format+"\r\n", args...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var batchSeq atomic.Int64

// deliverMultiline sends a finished multiline message on: as one batch to
// those who asked for multiline, and line by line to everyone else, as Ergo
// does.
func (c *client) deliverMultiline(b *inBatch) {
	s := c.srv
	s.mu.Lock()
	ch := s.channels[strings.ToLower(b.target)]
	var to []*client
	if ch != nil {
		for m := range ch.members {
			if m != c {
				to = append(to, m)
			}
		}
		for _, l := range b.lines {
			ch.history = append(ch.history, stored{at: time.Now().UTC(), line: fmt.Sprintf(":%s PRIVMSG %s :%s", c.prefix(), b.target, l.text)})
		}
	} else if dm := s.nicks[strings.ToLower(b.target)]; dm != nil {
		to = append(to, dm)
	}
	s.mu.Unlock()
	for _, r := range to {
		r.wmu.Lock()
		multiline := r.caps["draft/multiline"] && r.caps["batch"]
		r.wmu.Unlock()
		if !multiline {
			for _, l := range b.lines {
				r.sendFrom(c, fmt.Sprintf(":%s PRIVMSG %s :%s", c.prefix(), b.target, l.text))
			}
			continue
		}
		ref := fmt.Sprintf("b%d", batchSeq.Add(1))
		r.send(":%s BATCH +%s draft/multiline %s", c.prefix(), ref, b.target)
		for _, l := range b.lines {
			tags := "@batch=" + ref
			if l.concat {
				tags += ";draft/multiline-concat"
			}
			r.sendTagged(c, tags, fmt.Sprintf(":%s PRIVMSG %s :%s", c.prefix(), b.target, l.text))
		}
		r.send(":%s BATCH -%s", serverName, ref)
	}
}

// sendTagged is sendFrom with extra tags ("@a=b;c").
func (c *client) sendTagged(sender *client, tags, line string) {
	sender.wmu.Lock()
	account := sender.account
	sender.wmu.Unlock()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if account != "" && c.caps["account-tag"] {
		tags += ";account=" + account
	}
	fmt.Fprintf(c.conn, "%s %s\r\n", tags, line)
}
