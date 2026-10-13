// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

const (
	// idleAfter is how long a desk may go unused before it's stopped.
	idleAfter = 10 * time.Minute
	// defaultTimeout and maxTimeout are how long a command may run.
	defaultTimeout = 2 * time.Minute
	maxTimeout     = 10 * time.Minute
	// maxOutput is how much of a command's output is kept.
	maxOutput = 64 << 10
	// maxFile is the largest file read or written in one call.
	maxFile = 8 << 20
	// idleBefore is how long a desk must have gone unused before it's
	// stopped to make room for another: an agent between steps (the model
	// thinking) has gone a little while; one that's gone this long is done.
	idleBefore = 2 * time.Minute
)

// ErrFull is every desk the VM has room for running, and at work.
var ErrFull = errors.New("the sandbox is full: every desk it has room for is working; try again shortly")

// Options configure a Server.
type Options struct {
	Backend Backend
	// PublicKeys check networks' tokens, which the platform signs: one, or
	// the old and the new while the key's changed.
	PublicKeys []ed25519.PublicKey
	// AcceptV1 takes old tokens too (a network's name, signed, for ever),
	// for one release while runners move to new ones.
	AcceptV1 bool
	Now      func() time.Time // nil for the clock
	Logger   *log.Logger      // the audit log; nil for none
	// MaxRunning is how many desks may run at once, for the VM's memory:
	// past it, the desk left idle longest is stopped to make room, or, if
	// every one is at work, ErrFull. 0 for no limit.
	MaxRunning int
	// MaxPerNetwork is how many of those one network's desks may be, so
	// one network can't hold every one; 0 means MaxRunning.
	MaxPerNetwork int
	// DefaultStorageGB is how much a network's desks' files may take, in
	// all, when its token doesn't say (a Sized backend); 0 for no limit.
	DefaultStorageGB int
}

// Server is sandboxd's API.
type Server struct {
	opts  Options
	mux   *http.ServeMux
	mu    sync.Mutex
	desks map[Desk]*state
	since time.Time // when everything was last idle, if no desk has been used
	// removed is when each network's desks were last all removed (it was
	// deleted): no token signed before then works for it again, not even
	// for a network made later with its name.
	removed map[string]time.Time
	// storage is each network's limit as set, in GB.
	storage map[string]int
}

// claims are what the request's token says, if it's good for scope.
func (s *Server) claims(r *http.Request, scope string) (Claims, bool) {
	now := s.opts.Now()
	c, ok := claimsOf(s.opts.PublicKeys, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), now, s.opts.AcceptV1)
	if !ok || !c.may(scope) {
		return Claims{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for network, at := range s.removed {
		if now.Sub(at) > tokenLongest+2*tokenSkew {
			delete(s.removed, network) // every token from before has run out
		}
	}
	if at, gone := s.removed[c.Network]; gone && (c.Scope == "v1" || !c.Issued.After(at)) {
		return Claims{}, false
	}
	return c, true
}

// state is what sandboxd knows of a desk while it runs.
type state struct {
	awake    bool      // its container is running, as far as we know
	used     time.Time // when it was last used
	busy     int       // calls in progress
	stopping bool
	starting bool // being started: counts as running
}

// NewServer is sandboxd's API over a backend.
func NewServer(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{opts: o, mux: http.NewServeMux(), desks: map[Desk]*state{}, since: o.Now(), removed: map[string]time.Time{}, storage: map[string]int{}}
	s.mux.HandleFunc("POST /v1/desks/{nick}/open", s.desk(s.open))
	s.mux.HandleFunc("POST /v1/desks/{nick}/run", s.desk(s.run))
	s.mux.HandleFunc("POST /v1/desks/{nick}/read", s.desk(s.read))
	s.mux.HandleFunc("POST /v1/desks/{nick}/write", s.desk(s.write))
	s.mux.HandleFunc("DELETE /v1/desks/{nick}", s.desk(s.remove))
	s.mux.HandleFunc("DELETE /v1/desks", s.removeNetwork)
	// For the VM's power-off timer, on the VM: says nothing of any desk.
	s.mux.HandleFunc("GET /v1/idle", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]int{"idle_seconds": int(s.Idle() / time.Second)})
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logger != nil {
		s.opts.Logger.Printf(format, args...)
	}
}

type handler func(w http.ResponseWriter, r *http.Request, d Desk) error

// desk checks who's asking and for which desk, keeps track of the desk's
// use, and answers errors as JSON.
func (s *Server) desk(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.claims(r, Desks)
		if !ok {
			jsonError(w, http.StatusUnauthorized, "that isn't a good sandbox token for a network's desks: ask the platform for a new one")
			return
		}
		network := c.Network
		nick := strings.ToLower(r.PathValue("nick"))
		if !validName.MatchString(nick) {
			jsonError(w, http.StatusBadRequest, "desks are named by nick: lowercase letters, digits, - and _, up to 32")
			return
		}
		d := Desk{Network: network, Nick: nick}
		s.begin(d)
		defer s.end(d)
		r.Body = http.MaxBytesReader(w, r.Body, maxFile*2)
		err := s.limitStorage(r.Context(), c)
		if err == nil {
			err = h(w, r, d)
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrNoFile):
				jsonError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, ErrTooBig):
				jsonError(w, http.StatusRequestEntityTooLarge, err.Error())
			case errors.Is(err, ErrFull):
				w.Header().Set("Retry-After", "15")
				jsonError(w, http.StatusTooManyRequests, err.Error())
			case errors.As(err, new(badRequest)):
				jsonError(w, http.StatusBadRequest, err.Error())
			default:
				monitor.Problem(s.opts.Logger, "%s %s: %v", d.Network, d.Nick, err)
				jsonError(w, http.StatusInternalServerError, err.Error())
			}
		}
	}
}

// limitStorage sets the network's storage before its desks are used: as
// its token says, or the default for a network not yet limited. It's set
// again only when it changes.
func (s *Server) limitStorage(ctx context.Context, c Claims) error {
	b, ok := s.opts.Backend.(Sized)
	if !ok {
		return nil
	}
	s.mu.Lock()
	have, set := s.storage[c.Network]
	s.mu.Unlock()
	gb := c.StorageGB
	if gb == 0 {
		if set || s.opts.DefaultStorageGB <= 0 {
			return nil
		}
		gb = s.opts.DefaultStorageGB
	}
	if set && have == gb {
		return nil
	}
	if err := b.SetStorage(ctx, c.Network, gb); err != nil {
		return fmt.Errorf("limiting the network's storage to %d GB: %w", gb, err)
	}
	s.mu.Lock()
	s.storage[c.Network] = gb
	s.mu.Unlock()
	s.logf("%s: storage %d GB", c.Network, gb)
	return nil
}

type badRequest struct{ error }

func bad(format string, args ...any) error { return badRequest{fmt.Errorf(format, args...)} }

func (s *Server) begin(d Desk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.desks[d]
	if st == nil {
		st = &state{}
		s.desks[d] = st
	}
	st.busy++
	st.used = s.opts.Now()
}

func (s *Server) end(d Desk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.desks[d]; st != nil {
		st.busy--
		st.used = s.opts.Now()
	}
}

// awake makes sure d's container runs: after sandboxd starts, or once the
// desk was stopped for being idle, the first call wakes it. check asks the
// backend even if sandboxd thinks it runs (opening, at each wake-up).
func (s *Server) awake(ctx context.Context, d Desk, check bool) error {
	s.mu.Lock()
	st := s.desks[d]
	up := st != nil && st.awake
	s.mu.Unlock()
	if up && !check {
		return nil
	}
	if !up {
		if err := s.admit(ctx, d); err != nil {
			return err
		}
	}
	err := s.opts.Backend.Ensure(ctx, d)
	s.mu.Lock()
	if st := s.desks[d]; st != nil {
		st.starting = false
		st.awake = st.awake || err == nil
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("readying the desk: %w", err)
	}
	return nil
}

// admit finds room for d to start: below MaxRunning and its network's
// share (MaxPerNetwork), or by stopping the desk left idle longest
// (idleBefore at least, and not at work): its own network's, if it's at
// its share. d is then counted as starting. ErrFull if there's none to
// stop.
func (s *Server) admit(ctx context.Context, d Desk) error {
	if s.opts.MaxRunning <= 0 {
		return nil
	}
	share := s.opts.MaxPerNetwork
	if share <= 0 || share > s.opts.MaxRunning {
		share = s.opts.MaxRunning
	}
	for {
		now := s.opts.Now()
		s.mu.Lock()
		running, ours := 0, 0
		for k, st := range s.desks {
			if k != d && (st.awake || st.starting) {
				running++
				if k.Network == d.Network {
					ours++
				}
			}
		}
		atShare := ours >= share
		var idle Desk
		var idleSince time.Time
		found := false
		for k, st := range s.desks {
			if k == d || !(st.awake || st.starting) || (atShare && k.Network != d.Network) {
				continue
			}
			if st.awake && !st.starting && !st.stopping && st.busy == 0 && now.Sub(st.used) >= idleBefore &&
				(!found || st.used.Before(idleSince)) {
				idle, idleSince, found = k, st.used, true
			}
		}
		if running < s.opts.MaxRunning && !atShare {
			if st := s.desks[d]; st != nil {
				st.starting = true
			}
			s.mu.Unlock()
			return nil
		}
		if !found {
			s.mu.Unlock()
			s.logf("%s %s: no room (%d desks running, all at work)", d.Network, d.Nick, running)
			return ErrFull
		}
		s.desks[idle].stopping = true
		s.mu.Unlock()
		err := s.opts.Backend.Stop(ctx, idle)
		s.mu.Lock()
		if st := s.desks[idle]; st != nil {
			st.stopping = false
			if err == nil && st.busy == 0 {
				st.awake = false
			}
		}
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("making room: stopping %s %s: %w", idle.Network, idle.Nick, err)
		}
		s.logf("%s %s: stopped, to make room for %s %s", idle.Network, idle.Nick, d.Network, d.Nick)
	}
}

func (s *Server) open(w http.ResponseWriter, r *http.Request, d Desk) error {
	if err := s.awake(r.Context(), d, true); err != nil {
		return err
	}
	s.logf("%s %s: open", d.Network, d.Nick)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type runRequest struct {
	Args      []string `json:"args"`
	Dir       string   `json:"dir"`
	Stdin     string   `json:"stdin"`
	TimeoutMS int64    `json:"timeout_ms"`
}

type runResponse struct {
	Output   string `json:"output"`
	Exit     int    `json:"exit"`
	TimedOut bool   `json:"timed_out"`
}

func (s *Server) run(w http.ResponseWriter, r *http.Request, d Desk) error {
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Args) == 0 {
		return bad("give the command's args")
	}
	if req.Dir == "" {
		req.Dir = "/workspace"
	}
	if !path.IsAbs(req.Dir) {
		return bad("dir must be an absolute path")
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	switch {
	case timeout <= 0:
		timeout = defaultTimeout
	case timeout > maxTimeout:
		timeout = maxTimeout
	}
	if err := s.awake(r.Context(), d, false); err != nil {
		return err
	}
	out := &capped{max: maxOutput}
	e := Exec{Args: req.Args, Dir: req.Dir, Output: out, Timeout: timeout}
	if req.Stdin != "" {
		e.Stdin = strings.NewReader(req.Stdin)
	}
	start := s.opts.Now()
	res, err := s.opts.Backend.Exec(r.Context(), d, e)
	if err != nil {
		return err
	}
	s.logf("%s %s: run %s (%s, exit %d)", d.Network, d.Nick, short(strings.Join(req.Args, " "), 200), s.opts.Now().Sub(start).Round(time.Millisecond), res.Exit)
	writeJSON(w, runResponse{Output: out.String(), Exit: res.Exit, TimedOut: res.TimedOut})
	return nil
}

type fileRequest struct {
	Path    string `json:"path"`
	Max     int64  `json:"max,omitempty"`
	Content []byte `json:"content,omitempty"`
}

func (s *Server) read(w http.ResponseWriter, r *http.Request, d Desk) error {
	var req fileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !path.IsAbs(req.Path) {
		return bad("give the file's absolute path")
	}
	if req.Max <= 0 || req.Max > maxFile {
		req.Max = maxFile
	}
	if err := s.awake(r.Context(), d, false); err != nil {
		return err
	}
	data, err := s.opts.Backend.ReadFile(r.Context(), d, path.Clean(req.Path), req.Max)
	if err != nil {
		return err
	}
	writeJSON(w, fileRequest{Path: req.Path, Content: data})
	return nil
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, d Desk) error {
	var req fileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !path.IsAbs(req.Path) {
		return bad("give the file's absolute path")
	}
	if len(req.Content) > maxFile {
		return ErrTooBig
	}
	if err := s.awake(r.Context(), d, false); err != nil {
		return err
	}
	if err := s.opts.Backend.WriteFile(r.Context(), d, path.Clean(req.Path), req.Content); err != nil {
		return err
	}
	s.logf("%s %s: write %s (%d bytes)", d.Network, d.Nick, short(req.Path, 200), len(req.Content))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, d Desk) error {
	if err := s.opts.Backend.Remove(r.Context(), d); err != nil {
		return err
	}
	s.mu.Lock()
	if st := s.desks[d]; st != nil {
		st.awake = false
	}
	s.mu.Unlock()
	s.logf("%s %s: removed", d.Network, d.Nick)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// removeNetwork deletes every desk of the token's network, and their files:
// the platform asks once the network is deleted.
func (s *Server) removeNetwork(w http.ResponseWriter, r *http.Request) {
	c, ok := s.claims(r, Admin)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "only the platform removes a network's desks, with a token for that")
		return
	}
	network := c.Network
	if err := s.opts.Backend.RemoveNetwork(r.Context(), network); err != nil {
		monitor.Problem(s.opts.Logger, "%s: removing its desks: %v", network, err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	for d := range s.desks {
		if d.Network == network {
			delete(s.desks, d)
		}
	}
	s.removed[network] = s.opts.Now()
	delete(s.storage, network) // its files have gone, and their limit
	s.mu.Unlock()
	s.logf("%s: every desk removed", network)
	w.WriteHeader(http.StatusNoContent)
}

// StopIdle stops the desks nobody has used for a while, and isn't using now.
func (s *Server) StopIdle(ctx context.Context) {
	now := s.opts.Now()
	var idle []Desk
	s.mu.Lock()
	for d, st := range s.desks {
		if st.awake && st.busy == 0 && !st.stopping && now.Sub(st.used) >= idleAfter {
			st.stopping = true
			idle = append(idle, d)
		}
	}
	s.mu.Unlock()
	for _, d := range idle {
		err := s.opts.Backend.Stop(ctx, d)
		s.mu.Lock()
		if st := s.desks[d]; st != nil {
			st.stopping = false
			if err == nil && st.busy == 0 {
				st.awake = false
			}
		}
		s.mu.Unlock()
		if err != nil {
			monitor.Problem(s.opts.Logger, "%s %s: stopping: %v", d.Network, d.Nick, err)
		} else {
			s.logf("%s %s: stopped, idle", d.Network, d.Nick)
		}
	}
}

// Idle is how long no desk has been used: none while one is working.
func (s *Server) Idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.since
	for _, st := range s.desks {
		if st.busy > 0 {
			return 0
		}
		if st.used.After(last) {
			last = st.used
		}
	}
	return s.opts.Now().Sub(last)
}

// capped keeps the first max bytes written to it, and counts the rest.
type capped struct {
	mu   sync.Mutex
	b    strings.Builder
	max  int
	more int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - c.b.Len()
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		c.b.Write(p[:room])
	}
	c.more += len(p) - max(room, 0)
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.more == 0 {
		return c.b.String()
	}
	return c.b.String() + fmt.Sprintf("\n[output cut short: %d more bytes. Ask for less, e.g. pipe it through head or tail.]", c.more)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
