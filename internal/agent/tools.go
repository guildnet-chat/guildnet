// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
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
// that it can is in security.md.
const YourRepos = "your repos"

// Sourced is a toolbox that says where its tools' content comes from
// ("GitHub", "Linear"), so that a joined toolbox can tell a write soon
// after reading somewhere else.
type Sourced interface {
	SourceOf(name string) string
}

// Join is toolboxes as one (GitHub's and the connectors', say): each call
// goes to the toolbox that has the tool. Nils are left out; none left is
// nil, and one is itself. Joined toolboxes join as theirs, so reads are
// kept track of across all of them.
func Join(boxes ...Toolbox) Toolbox {
	j := &joined{read: map[string]map[string]time.Time{}}
	for _, b := range boxes {
		switch b := b.(type) {
		case nil:
		case *joined:
			j.boxes = append(j.boxes, b.boxes...)
		default:
			j.boxes = append(j.boxes, b)
		}
	}
	switch len(j.boxes) {
	case 0:
		return nil
	case 1:
		return j.boxes[0]
	}
	return j
}

type joined struct {
	boxes []Toolbox

	mu   sync.Mutex
	read map[string]map[string]time.Time // agent -> source -> when it last read there
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
		return &joined{boxes: []Toolbox{tb}, read: map[string]map[string]time.Time{}}
	}
}

func (j *joined) ReadFrom(p config.Persona, source string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.read[p.Nick] == nil {
		j.read[p.Nick] = map[string]time.Time{}
	}
	j.read[p.Nick][source] = time.Now()
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

func sourceOf(b Toolbox, name string) string {
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
	if src := sourceOf(b, name); src != "" {
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
	src := sourceOf(b, name)
	if tool.ReadOnly || tool.Ask || src == "" {
		return false, ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var elsewhere []string
	for read, at := range j.read[p.Nick] {
		if read != src && time.Since(at) < ReadWindow {
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
