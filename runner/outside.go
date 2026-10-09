// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/chat"
)

// Agents built outside GuildNet (a LangChain script, a CrewAI crew) join as
// employees: GuildNet keeps their IRC presence, when they wake, and the org
// chart, and their own program does the thinking. It pulls its wake-ups
// from an inbox and answers them, through the platform, which passes calls
// on with the network's secret; the agent's own token says which agent is
// calling.

// PlatformOutside is the platform value for agents that run outside
// GuildNet.
const PlatformOutside = "outside"

const (
	// connectedFor is how long after its program last checked in an outside
	// agent still counts as connected.
	connectedFor = 2 * time.Minute
	// maxInboxWait is the longest one check of the inbox is held open.
	maxInboxWait = 25 * time.Second
	// maxAgentText is the most an outside agent may send in one call.
	maxAgentText = 4 << 10
	// agentTokenHeader carries an outside agent's token, passed on by the
	// platform.
	agentTokenHeader = "GuildNet-Agent-Token"
)

// defaultDailyWakes is how many times a day an outside agent may be woken
// unless it's given another number.
const defaultDailyWakes = 100

// wakeCap is how many times a day p may be woken.
func wakeCap(p config.Persona) int {
	if p.DailyWakes > 0 {
		return p.DailyWakes
	}
	return defaultDailyWakes
}

// errNotConnected means an outside agent's program isn't checking in.
var errNotConnected = errors.New("its program outside GuildNet isn't checking in for work")

// wakeLine is a line of conversation, as outside agents get it.
type wakeLine struct {
	From   string    `json:"from"`
	Text   string    `json:"text"`
	Time   time.Time `json:"time"`
	Action bool      `json:"action,omitempty"`
}

func wakeLineOf(l chat.Line) wakeLine {
	return wakeLine{From: l.Nick, Text: l.Text, Time: l.At, Action: l.Action}
}

// wakeUp is one wake-up of an outside agent, as its program gets it: the
// same as GuildNet's own agents get, structured, and as ready-made words
// for any model.
type wakeUp struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"` // wake
	Where      string     `json:"where"`
	Channel    string     `json:"channel,omitempty"`
	DMWith     string     `json:"dm_with,omitempty"`
	Trigger    wakeLine   `json:"trigger"`
	Transcript []wakeLine `json:"transcript"`
	Topic      string     `json:"topic,omitempty"`
	Goals      string     `json:"goals,omitempty"`
	Team       string     `json:"team,omitempty"`
	Prompt     struct {
		System string `json:"system"`
		User   string `json:"user"`
	} `json:"prompt"`
	ReplyBy time.Time `json:"reply_by"`
	// Approval is the decision on something the agent asked for (type
	// approval).
	Approval *approvalNews `json:"approval,omitempty"`
}

// answer is how a wake-up ended: a reply, or giving up.
type answer struct {
	reply, gaveUp string
}

// waiting is a wake-up an outside agent hasn't answered yet.
type waiting struct {
	agent  string // its key
	wake   wakeUp
	onStep func(agent.Step)
	done   chan answer
}

// inboxes hold outside agents' wake-ups until their programs collect and
// answer them.
type inboxes struct {
	mu      sync.Mutex
	queued  map[string][]*waiting // by agent, not yet collected
	open    map[string]*waiting   // by wake-up id, collected or not
	seen    map[string]time.Time  // when each agent last checked in
	polling map[string]int        // checks each agent has waiting now
	arrived map[string]chan struct{}
	woken   map[string]dayCount // wake-ups today, by agent
	posted  map[string]dayCount // messages this minute, by agent (day is the minute)
	hooked  map[string]delivery // how each agent's webhook last went
}

// post counts a message an agent posts, or says it's one too many this
// minute.
func (in *inboxes) post(nick string, now time.Time) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	k, minute := strings.ToLower(nick), now.UTC().Format("2006-01-02T15:04")
	c := in.posted[k]
	if c.day != minute {
		c = dayCount{day: minute}
	}
	if c.n >= agentMessagesAMinute {
		return false
	}
	c.n++
	in.posted[k] = c
	return true
}

// current is the wake-up an agent's program is working on (collected, not
// yet answered), or nil.
func (in *inboxes) current(nick string) *waiting {
	in.mu.Lock()
	defer in.mu.Unlock()
	k := strings.ToLower(nick)
	var out *waiting
	for _, x := range in.open {
		if x.agent == k && !queuedIn(in.queued[k], x) && (out == nil || x.wake.ReplyBy.After(out.wake.ReplyBy)) {
			out = x
		}
	}
	return out
}

func queuedIn(q []*waiting, x *waiting) bool {
	for _, y := range q {
		if y == x {
			return true
		}
	}
	return false
}

// wokenInDM says whether an agent has a wake-up open from a DM with peer.
func (in *inboxes) wokenInDM(nick, peer string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	k := strings.ToLower(nick)
	for _, x := range in.open {
		if x.agent == k && x.wake.DMWith != "" && strings.EqualFold(x.wake.DMWith, peer) {
			return true
		}
	}
	return false
}

type dayCount struct {
	day string
	n   int
}

func newInboxes() *inboxes {
	return &inboxes{queued: map[string][]*waiting{}, open: map[string]*waiting{}, seen: map[string]time.Time{},
		polling: map[string]int{}, arrived: map[string]chan struct{}{}, woken: map[string]dayCount{}, posted: map[string]dayCount{}, hooked: map[string]delivery{}}
}

// wakesOn is how many times an agent has been woken on day.
func (in *inboxes) wakesOn(nick, day string) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	if c := in.woken[strings.ToLower(nick)]; c.day == day {
		return c.n
	}
	return 0
}

// countWake counts a wake-up on day.
func (in *inboxes) countWake(nick, day string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	k := strings.ToLower(nick)
	c := in.woken[k]
	if c.day != day {
		c = dayCount{day: day}
	}
	c.n++
	in.woken[k] = c
}

// connected says whether an agent's program is checking in.
func (in *inboxes) connected(nick string, now time.Time) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	k := strings.ToLower(nick)
	return in.polling[k] > 0 || now.Sub(in.seen[k]) < connectedFor
}

// lastSeen is when an agent's program last checked in.
func (in *inboxes) lastSeen(nick string) time.Time {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.seen[strings.ToLower(nick)]
}

func (in *inboxes) signal(k string) {
	if ch, ok := in.arrived[k]; ok {
		close(ch)
		delete(in.arrived, k)
	}
}

// deliver puts w in its agent's inbox and waits for the answer, or until
// ctx ends.
func (in *inboxes) deliver(ctx context.Context, nick string, w wakeUp, onStep func(agent.Step)) (answer, error) {
	k := strings.ToLower(nick)
	x := &waiting{agent: k, wake: w, onStep: onStep, done: make(chan answer, 1)}
	in.mu.Lock()
	in.queued[k] = append(in.queued[k], x)
	in.open[w.ID] = x
	in.signal(k)
	in.mu.Unlock()
	defer func() {
		in.mu.Lock()
		delete(in.open, w.ID)
		q := in.queued[k]
		for i, y := range q {
			if y == x {
				in.queued[k] = append(q[:i:i], q[i+1:]...)
				break
			}
		}
		in.mu.Unlock()
	}()
	select {
	case a := <-x.done:
		return a, nil
	case <-ctx.Done():
		return answer{}, fmt.Errorf("its program outside GuildNet didn't answer in time: %w", ctx.Err())
	}
}

// collect is an agent's program checking its inbox: the wake-ups waiting,
// as soon as there are any, or none after wait.
func (in *inboxes) collect(ctx context.Context, nick string, wait time.Duration, now func() time.Time) []wakeUp {
	k := strings.ToLower(nick)
	in.mu.Lock()
	in.polling[k]++
	in.seen[k] = now()
	in.mu.Unlock()
	defer func() {
		in.mu.Lock()
		in.polling[k]--
		in.seen[k] = now()
		in.mu.Unlock()
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		in.mu.Lock()
		if q := in.queued[k]; len(q) > 0 {
			out := make([]wakeUp, 0, len(q))
			for _, x := range q {
				out = append(out, x.wake)
			}
			delete(in.queued, k)
			in.mu.Unlock()
			return out
		}
		ch, ok := in.arrived[k]
		if !ok {
			ch = make(chan struct{})
			in.arrived[k] = ch
		}
		in.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			return []wakeUp{}
		case <-ctx.Done():
			return []wakeUp{}
		}
	}
}

// find is an open wake-up of this agent's, or nil.
func (in *inboxes) find(nick, id string) *waiting {
	in.mu.Lock()
	defer in.mu.Unlock()
	x := in.open[id]
	if x == nil || x.agent != strings.ToLower(nick) {
		return nil
	}
	return x
}

// register opens a wake-up sent some other way than the inbox (a webhook),
// so the agent can answer it through the API.
func (in *inboxes) register(nick string, w wakeUp, onStep func(agent.Step)) *waiting {
	x := &waiting{agent: strings.ToLower(nick), wake: w, onStep: onStep, done: make(chan answer, 1)}
	in.mu.Lock()
	in.open[w.ID] = x
	in.mu.Unlock()
	return x
}

// forget closes a registered wake-up.
func (in *inboxes) forget(id string) {
	in.mu.Lock()
	delete(in.open, id)
	in.mu.Unlock()
}

// delivery is how the last call to an agent's webhook went.
type delivery struct {
	At  time.Time `json:"at"`
	OK  bool      `json:"ok"`
	Err string    `json:"error,omitempty"`
}

// delivered notes how a call to an agent's webhook went.
func (in *inboxes) delivered(nick string, err error) {
	d := delivery{At: time.Now().UTC(), OK: err == nil}
	if err != nil {
		d.Err = err.Error()
	}
	in.mu.Lock()
	in.hooked[strings.ToLower(nick)] = d
	in.mu.Unlock()
}

// lastDelivery is how the last call to an agent's webhook went, if any.
func (in *inboxes) lastDelivery(nick string) (delivery, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	d, ok := in.hooked[strings.ToLower(nick)]
	return d, ok
}

// finish ends an open wake-up with a, once.
func (in *inboxes) finish(nick, id string, a answer) bool {
	in.mu.Lock()
	x := in.open[id]
	if x == nil || x.agent != strings.ToLower(nick) {
		in.mu.Unlock()
		return false
	}
	delete(in.open, id)
	in.mu.Unlock()
	x.done <- a
	return true
}

// outsideStatus says whether an outside agent's program is checking in, in
// words.
func (r *Runner) outsideStatus(nick string) string {
	if r.outside.connected(nick, r.now()) {
		return "connected"
	}
	seen := r.outside.lastSeen(nick)
	if seen.IsZero() {
		return "not connected (its program hasn't checked in since GuildNet started)"
	}
	return "not connected (its program last checked in " + seen.In(r.loc()).Format("Mon Jan 2 15:04 MST") + ")"
}

// respondOutside wakes an outside agent: its wake-up goes to its inbox,
// and what its program answers is its reply.
func (r *Runner) respondOutside(ctx context.Context, req agent.Request, channel, peer string, onStep func(agent.Step)) (agent.Result, error) {
	p := req.Persona
	w := wakeUp{ID: "wk_" + randomID(), Type: "wake", Where: req.Where, Channel: channel, DMWith: peer,
		Trigger: wakeLineOf(req.Trigger), Topic: req.Topic, Goals: req.Goals, Team: req.Team, Transcript: []wakeLine{}}
	if channel == "" {
		w.Where = "dm:" + peer
	}
	for _, l := range req.Transcript {
		w.Transcript = append(w.Transcript, wakeLineOf(l))
	}
	w.Prompt.System = agent.SystemPrompt(r.cfg.Company, r.cfg.Owner, p, agent.Outside)
	w.Prompt.User = agent.WakePrompt(req)
	if deadline, ok := ctx.Deadline(); ok {
		w.ReplyBy = deadline.UTC()
	}
	a, err := r.deliverWake(ctx, p, w, onStep)
	if !errors.Is(err, errNotConnected) { // it was woken, whatever came of it
		r.outside.countWake(p.Nick, r.today())
	}
	if err != nil {
		return agent.Result{}, err
	}
	if a.gaveUp != "" {
		return agent.Result{}, fmt.Errorf("its program outside GuildNet gave up: %s", a.gaveUp)
	}
	return agent.Result{Reply: a.reply}, nil
}

// today is the date in the company's timezone.
func (r *Runner) today() string { return r.now().In(r.loc()).Format("2006-01-02") }

// outsideCapped says why an outside agent can't be woken again today, if it
// can't.
func (r *Runner) outsideCapped(p config.Persona) string {
	if p.Platform != PlatformOutside {
		return ""
	}
	if n := wakeCap(p); r.outside.wakesOn(p.Nick, r.today()) >= n {
		return fmt.Sprintf("it has had its %d wake-ups for today (WAKES sets how many)", n)
	}
	return ""
}

// respond wakes an agent on its platform: outside agents through their
// inbox, the rest through the brain.
func (r *Runner) respond(ctx context.Context, req agent.Request, channel, peer string, onStep func(agent.Step)) (agent.Result, error) {
	if req.Persona.Platform == PlatformOutside {
		return r.respondOutside(ctx, req, channel, peer, onStep)
	}
	return r.brain.Respond(ctx, req, onStep)
}

// outsideReady says why agents can't be moved outside, if they can't: they
// reach GuildNet through the platform, which calls the runner's API.
func (r *Runner) outsideReady() string {
	if r.cfg.API.Listen == "" {
		return "agents can't run outside GuildNet on this network: it has no API for them to reach."
	}
	return ""
}

// The API outside agents' programs call (through the platform).

type agentKey struct{}

func agentOf(req *http.Request) string {
	a, _ := req.Context().Value(agentKey{}).(string)
	return a
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// agentAuth lets a call through only with the network's secret (it came
// through the platform) and an outside agent's token, and says which agent
// it is.
func (r *Runner) agentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		nick, status, why := r.outsideCaller(req)
		if status != 0 {
			apiError(w, status, why)
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), agentKey{}, nick)))
	})
}

// outsideCaller is the outside agent calling, or the status and why not:
// only with the network's secret (the call came through the platform) and
// the token of an agent that runs outside GuildNet now.
func (r *Runner) outsideCaller(req *http.Request) (nick string, status int, why string) {
	secret, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(secret), []byte(r.cfg.API.Token)) != 1 {
		return "", http.StatusUnauthorized, "this network's secret is required"
	}
	token := strings.TrimSpace(req.Header.Get(agentTokenHeader))
	err := errors.New("no token")
	if strings.HasPrefix(token, "gna_") {
		nick, err = r.store.AgentForToken(req.Context(), hashToken(token))
	}
	if err != nil {
		return "", http.StatusUnauthorized, "that isn't an agent's token here"
	}
	if p, err := r.store.Agent(req.Context(), nick); err != nil || p.Platform != PlatformOutside {
		return "", http.StatusForbidden, nick + " doesn't run outside GuildNet now"
	}
	return nick, 0, ""
}

// agentRoutes are the calls outside agents' programs make.
func (r *Runner) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /agents/v1/me", func(w http.ResponseWriter, req *http.Request) {
		p, err := r.store.Agent(req.Context(), agentOf(req))
		if err != nil {
			apiRefusal(w, r.storeRefusal(agentOf(req), err))
			return
		}
		getsWork := "inbox"
		if url, _, _ := r.store.AgentWebhook(req.Context(), p.Nick); url != "" {
			getsWork = "webhook"
		}
		apiJSON(w, http.StatusOK, map[string]any{"nick": p.Nick, "name": p.Name, "role": p.Role, "team": p.Team,
			"reports_to": p.ReportsTo, "channels": p.Channels, "gets_work": getsWork})
	})
	mux.HandleFunc("GET /agents/v1/inbox", func(w http.ResponseWriter, req *http.Request) {
		wait := maxInboxWait
		if s := req.URL.Query().Get("wait"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				apiError(w, http.StatusBadRequest, "wait is seconds, 0 to 25")
				return
			}
			wait = min(time.Duration(n)*time.Second, maxInboxWait)
		}
		apiJSON(w, http.StatusOK, map[string]any{"wakes": r.outside.collect(req.Context(), agentOf(req), wait, r.now)})
	})
	mux.HandleFunc("POST /agents/v1/wakes/{id}/reply", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if !decode(w, req, &body) {
			return
		}
		text, ok := cleanAgentText(w, body.Text)
		if !ok {
			return
		}
		if text == "" {
			text = agent.Pass
		}
		r.finishWake(w, req, answer{reply: text})
	})
	mux.HandleFunc("POST /agents/v1/wakes/{id}/fail", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Reason string `json:"reason"`
		}
		if !decode(w, req, &body) {
			return
		}
		reason, ok := cleanAgentText(w, body.Reason)
		if !ok {
			return
		}
		if reason == "" {
			reason = "no reason given"
		}
		r.finishWake(w, req, answer{gaveUp: strings.Join(strings.Fields(reason), " ")})
	})
	mux.HandleFunc("POST /agents/v1/wakes/{id}/steps", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if !decode(w, req, &body) {
			return
		}
		text, ok := cleanAgentText(w, body.Text)
		if !ok {
			return
		}
		x := r.outside.find(agentOf(req), req.PathValue("id"))
		if x == nil {
			apiError(w, http.StatusNotFound, "no wake-up of yours is open with that id (answered, timed out, or never yours)")
			return
		}
		if line := strings.Join(strings.Fields(text), " "); line != "" {
			x.onStep(agent.Step{Summary: line})
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (r *Runner) finishWake(w http.ResponseWriter, req *http.Request, a answer) {
	if !r.outside.finish(agentOf(req), req.PathValue("id"), a) {
		apiError(w, http.StatusNotFound, "no wake-up of yours is open with that id (answered, timed out, or never yours)")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cleanAgentText is what an outside agent sent, safe to post: no control
// characters (so nothing can smuggle in IRC commands or CTCP), line breaks
// kept as line breaks, at most maxAgentText.
func cleanAgentText(w http.ResponseWriter, s string) (string, bool) {
	if len(s) > maxAgentText {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("that's %d bytes; the most is %d", len(s), maxAgentText))
		return "", false
	}
	return cleanText(s), true
}

// cleanText is s without control characters (tabs become spaces), line
// breaks kept.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	clean := strings.Map(func(c rune) rune {
		switch {
		case c == '\n':
			return c
		case c == '\t':
			return ' '
		case unicode.IsControl(c):
			return -1
		}
		return c
	}, s)
	return strings.TrimSpace(clean)
}

// newAgentToken makes an outside agent a new token, in place of any it
// had. The token itself is returned once and only its hash kept.
func (r *Runner) newAgentToken(ctx context.Context, actor, nick string) (string, error) {
	p, err := r.store.Agent(ctx, nick)
	if err != nil {
		return "", r.storeRefusal(nick, err)
	}
	if !r.can(actor, canHire, onPerson(p.Nick)) {
		return "", refuse(forbidden, "you can't change %s, so you can't give it a token.", p.Nick)
	}
	if p.Platform != PlatformOutside {
		return "", refuse(invalid, "%s doesn't run outside GuildNet, so it has no token. Move it outside first (PLATFORM outside).", p.Nick)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := "gna_" + base64.RawURLEncoding.EncodeToString(b)
	if err := r.store.SetAgentToken(ctx, p.Nick, hashToken(token)); err != nil {
		return "", r.storeRefusal(p.Nick, err)
	}
	r.logChange(actor, "new token", p.Nick, p.Team, "for its program outside GuildNet")
	return token, nil
}

func (r *Runner) apiAgentToken(w http.ResponseWriter, req *http.Request) {
	token, err := r.newAgentToken(req.Context(), actorOf(req), req.PathValue("nick"))
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]string{"token": token})
}
