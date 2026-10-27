// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/config"
)

// Tool is an action the runner carries out on an agent's behalf, with
// credentials the agent never sees. Anything that reaches outside the
// agent's own workspace (GitHub today) is a Tool, so GuildNet decides what
// happens and who has to approve it, whatever platform the agent runs on.
type Tool struct {
	Name        string
	Description string
	Properties  map[string]any // JSON Schema properties of the input object
	Required    []string
	Ask         bool // pauses for a person's approval before it runs
	// ReadOnly marks a tool that only looks (reading a file); what it found
	// isn't announced in IRC, only that it looked.
	ReadOnly bool
}

// Toolbox is a set of Tools and the code that runs them. What an agent gets
// can depend on who it is (its team, say): none is fine.
type Toolbox interface {
	Tools(p config.Persona) []Tool
	// Guide is what the agent is told about working with its tools.
	Guide(p config.Persona) string
	// Prepare readies a call that needs approval: it checks the call makes
	// sense, describes it for the approver (a verb phrase, e.g. "merge PR #17
	// ..."), and pins what's being approved, so that what runs later is what
	// the person saw. An error goes back to the agent instead of a person.
	Prepare(ctx context.Context, p config.Persona, name string, input map[string]any) (summary string, pinned map[string]any, err error)
	// Run carries out a call and returns what the agent is told. An error is
	// also told to the agent, as a failed tool call.
	Run(ctx context.Context, p config.Persona, name string, input map[string]any) (string, error)
}

// Asker is a toolbox some of whose calls must ask a person first although
// their tool doesn't usually (a write to one service soon after reading
// another's content, say); why is said to the approver.
type Asker interface {
	MustAsk(p config.Persona, name string, input map[string]any) (ask bool, why string)
}

// MustAsk says whether tb says this call must ask first.
func MustAsk(tb Toolbox, p config.Persona, name string, input map[string]any) bool {
	a, ok := tb.(Asker)
	if !ok {
		return false
	}
	ask, _ := a.MustAsk(p, name, input)
	return ask
}

// Untrusted wraps text read from source (a file, an issue, a command's
// output) so the agent knows it's content, not instructions: whoever wrote
// it may be anyone. The text can't end the wrapper early and speak as if
// outside it.
func Untrusted(source, text string) string {
	text = strings.ReplaceAll(text, "</untrusted", "<\\/untrusted")
	return fmt.Sprintf("<untrusted source=%q>\n%s\n</untrusted>", source, text)
}

// Headline is what a tool achieved, for IRC: the first line of what it gave,
// past the <untrusted> tag a connector's result starts with.
func Headline(out string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "</untrusted>" || (strings.HasPrefix(line, "<untrusted") && strings.HasSuffix(line, ">")) {
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return "" // data for the agent, not news for people
		}
		return strings.ToLower(line[:1]) + line[1:]
	}
	return ""
}

// ReadWindow is how long after reading from one place a write to another
// asks a person first: what was read there could be steering it.
const ReadWindow = 30 * time.Minute

// YourRepos is where the team's code comes from, wherever it's hosted: a
// desk's reads, GitHub's tools and pushes to other git hosts. Work read in
// one of the team's repositories may go to another of them without asking;
// that it can is in security.md. Its public GitHub repos are PublicRepos.
const YourRepos = "your repos"

// PublicRepos are the team's public GitHub repos: what's in them may have
// been written by anyone (a pull request from a fork, an issue), so they're
// a source of their own. Work read in a private repo goes to a public one
// only with a person's approval, and the other way round.
const PublicRepos = "public repos"

// Sourced is a toolbox that says where its tools' content comes from
// ("GitHub", "Linear"), so that a joined toolbox can tell a write soon
// after reading somewhere else.
type Sourced interface {
	SourceOf(name string) string
}

// CallSourced is a toolbox whose tools' source depends on the call: GitHub's,
// on whether the repo it names is public.
type CallSourced interface {
	SourceOfCall(name string, input map[string]any) string
}

// Join is toolboxes as one (GitHub's and the connectors', say): each call
// goes to the toolbox that has the tool. Nils are left out; none left is
// nil, and one is itself. Joined toolboxes join as theirs, so reads are
// kept track of across all of them (in the Reads one of them was given,
// if any: KeepReads).
func Join(boxes ...Toolbox) Toolbox {
	j := &joined{}
	for _, b := range boxes {
		switch b := b.(type) {
		case nil:
		case *joined:
			j.boxes = append(j.boxes, b.boxes...)
			if j.reads == nil && b.reads.shared {
				j.reads = b.reads
			}
		default:
			j.boxes = append(j.boxes, b)
		}
	}
	if j.reads == nil {
		j.reads = NewReads(nil)
		j.reads.shared = false
	}
	switch len(j.boxes) {
	case 0:
		return nil
	case 1:
		if j.reads.shared {
			return j // its Reads stay with it
		}
		return j.boxes[0]
	}
	return j
}

type joined struct {
	boxes []Toolbox
	reads *Reads
}

// ReadStore keeps settings (the runner's org store does): where Reads keeps
// what agents read.
type ReadStore interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Reads is when each agent last read from each source. One is shared by an
// agent's toolboxes (chat's, its desk's: KeepReads), and kept in a store,
// when there's one, so a restart doesn't forget it.
type Reads struct {
	store  ReadStore // nil: in memory only
	shared bool      // made by NewReads, for toolboxes to share
	mu     sync.Mutex
	read   map[string]map[string]time.Time // agent -> source -> when it last read there
}

// NewReads keeps what agents read in store (nil: in memory only).
func NewReads(store ReadStore) *Reads {
	return &Reads{store: store, shared: true, read: map[string]map[string]time.Time{}}
}

// KeepReads is tb keeping what its agents read in r, which other toolboxes
// may share; nil stays nil.
func KeepReads(tb Toolbox, r *Reads) Toolbox {
	switch tb := tb.(type) {
	case nil:
		return nil
	case *joined:
		return &joined{boxes: tb.boxes, reads: r}
	default:
		return &joined{boxes: []Toolbox{tb}, reads: r}
	}
}

func readsKey(nick string) string { return "reads/" + strings.ToLower(nick) }

// of is what nick read within ReadWindow, by source. Callers hold mu.
func (r *Reads) of(nick string) map[string]time.Time {
	k := strings.ToLower(nick)
	if _, ok := r.read[k]; !ok && r.store != nil {
		// Not since this process started: as the store keeps it.
		kept := map[string]time.Time{}
		if v, err := r.store.Setting(context.Background(), readsKey(nick)); err == nil && v != "" {
			json.Unmarshal([]byte(v), &kept)
		}
		r.read[k] = kept
	}
	if r.read[k] == nil {
		r.read[k] = map[string]time.Time{}
	}
	for source, at := range r.read[k] {
		if time.Since(at) >= ReadWindow {
			delete(r.read[k], source)
		}
	}
	return r.read[k]
}

func (r *Reads) readFrom(nick, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	read := r.of(nick)
	read[source] = time.Now()
	if r.store != nil {
		if b, err := json.Marshal(read); err == nil {
			r.store.SetSetting(context.Background(), readsKey(nick), string(b))
		}
	}
}

// Reader is a toolbox told when an agent read from somewhere its own tools
// don't reach (a Managed Agents desk's web fetch, say), so a write soon
// after asks first, as after reading with its tools.
type Reader interface {
	ReadFrom(p config.Persona, source string)
}

// Tracked is tb keeping track of what its agents read, as Join's toolboxes
// do, even on its own; nil stays nil.
func Tracked(tb Toolbox) Toolbox {
	switch tb := tb.(type) {
	case nil:
		return nil
	case *joined:
		return tb
	default:
		r := NewReads(nil)
		r.shared = false
		return &joined{boxes: []Toolbox{tb}, reads: r}
	}
}

func (j *joined) ReadFrom(p config.Persona, source string) {
	j.reads.readFrom(p.Nick, source)
}

func (j *joined) Tools(p config.Persona) []Tool {
	var out []Tool
	for _, b := range j.boxes {
		out = append(out, b.Tools(p)...)
	}
	return out
}

func (j *joined) Guide(p config.Persona) string {
	var parts []string
	for _, b := range j.boxes {
		if g := b.Guide(p); g != "" {
			parts = append(parts, g)
		}
	}
	return strings.Join(parts, "\n\n")
}

// owner is the toolbox with the tool called name, and the tool.
func (j *joined) owner(p config.Persona, name string) (Toolbox, Tool, error) {
	for _, b := range j.boxes {
		for _, t := range b.Tools(p) {
			if t.Name == name {
				return b, t, nil
			}
		}
	}
	return nil, Tool{}, fmt.Errorf("there's no tool called %s", name)
}

func sourceOf(b Toolbox, name string, input map[string]any) string {
	if s, ok := b.(CallSourced); ok {
		return s.SourceOfCall(name, input)
	}
	if s, ok := b.(Sourced); ok {
		return s.SourceOf(name)
	}
	return ""
}

func (j *joined) Prepare(ctx context.Context, p config.Persona, name string, input map[string]any) (string, map[string]any, error) {
	b, _, err := j.owner(p, name)
	if err != nil {
		return "", nil, err
	}
	return b.Prepare(ctx, p, name, input)
}

// Run runs the call in its toolbox. Whatever came back, the agent has now
// read something from where it went.
func (j *joined) Run(ctx context.Context, p config.Persona, name string, input map[string]any) (string, error) {
	b, _, err := j.owner(p, name)
	if err != nil {
		return "", err
	}
	if src := sourceOf(b, name, input); src != "" {
		j.ReadFrom(p, src)
	}
	return b.Run(ctx, p, name, input)
}

// MustAsk asks the toolbox with the tool; and a write that doesn't only read
// asks first if the agent read from somewhere else shortly before.
func (j *joined) MustAsk(p config.Persona, name string, input map[string]any) (bool, string) {
	b, tool, err := j.owner(p, name)
	if err != nil {
		return false, ""
	}
	if a, ok := b.(Asker); ok {
		if ask, why := a.MustAsk(p, name, input); ask {
			return ask, why
		}
	}
	src := sourceOf(b, name, input)
	if tool.ReadOnly || tool.Ask || src == "" {
		return false, ""
	}
	j.reads.mu.Lock()
	defer j.reads.mu.Unlock()
	var elsewhere []string
	for read := range j.reads.of(p.Nick) {
		if read != src {
			elsewhere = append(elsewhere, read)
		}
	}
	if len(elsewhere) == 0 {
		return false, ""
	}
	sort.Strings(elsewhere)
	return true, fmt.Sprintf("it read from %s shortly before, and what it read there could be steering it", strings.Join(elsewhere, " and "))
}

// Step describes a call in the words of the first toolbox that has some.
func (j *joined) Step(name string, input map[string]any) string {
	for _, b := range j.boxes {
		if s, ok := b.(interface {
			Step(name string, input map[string]any) string
		}); ok {
			if text := s.Step(name, input); text != "" {
				return text
			}
		}
	}
	return ""
}
