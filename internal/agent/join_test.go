// SPDX-License-Identifier: AGPL-3.0-only

package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// box is a toolbox with some tools, that says which of its tools ran.
type box struct {
	tools []agent.Tool
	guide string
	ran   *[]string
}

func (b box) Tools(config.Persona) []agent.Tool { return b.tools }
func (b box) Guide(config.Persona) string       { return b.guide }
func (b box) Prepare(_ context.Context, _ config.Persona, name string, in map[string]any) (string, map[string]any, error) {
	return "do " + name, in, nil
}
func (b box) Run(_ context.Context, _ config.Persona, name string, _ map[string]any) (string, error) {
	*b.ran = append(*b.ran, name)
	return "ran " + name, nil
}
func (b box) Step(name string, _ map[string]any) string {
	for _, t := range b.tools {
		if t.Name == name {
			return "steps " + name
		}
	}
	return ""
}

// GitHub's tools and the connectors' are one toolbox to agents: each call
// goes to the toolbox that has the tool.
func TestJoinedToolboxesAreOne(t *testing.T) {
	var ran []string
	github := box{tools: []agent.Tool{{Name: "github_read_file"}}, guide: "<github/>", ran: &ran}
	linear := box{tools: []agent.Tool{{Name: "linear_create_issue", Ask: true}}, guide: "<connectors/>", ran: &ran}
	tb := agent.Join(github, nil, linear)
	p := config.Persona{Nick: "sam"}

	if got := tb.Tools(p); len(got) != 2 || got[0].Name != "github_read_file" || got[1].Name != "linear_create_issue" {
		t.Errorf("tools %v, want both toolboxes'", got)
	}
	if g := tb.Guide(p); !strings.Contains(g, "<github/>") || !strings.Contains(g, "<connectors/>") {
		t.Errorf("guide %q, want both", g)
	}
	tb.Run(context.Background(), p, "linear_create_issue", nil)
	tb.Run(context.Background(), p, "github_read_file", nil)
	if strings.Join(ran, ",") != "linear_create_issue,github_read_file" {
		t.Errorf("ran %v", ran)
	}
	if s, _, _ := tb.Prepare(context.Background(), p, "linear_create_issue", nil); s != "do linear_create_issue" {
		t.Errorf("prepared %q", s)
	}
	if _, err := tb.Run(context.Background(), p, "jira_delete", nil); err == nil {
		t.Error("a tool no toolbox has should fail")
	}
	if s := tb.(interface {
		Step(string, map[string]any) string
	}).Step("linear_create_issue", nil); s != "steps linear_create_issue" {
		t.Errorf("step %q", s)
	}
}

type askingBox struct{ box }

func (askingBox) MustAsk(config.Persona, string, map[string]any) (bool, string) { return true, "why" }

// A joined toolbox asks the toolbox that has the tool whether a call must
// ask first.
func TestJoinedToolboxesPassOnMustAsk(t *testing.T) {
	var ran []string
	github := box{tools: []agent.Tool{{Name: "github_read_file"}}, ran: &ran}
	linear := askingBox{box{tools: []agent.Tool{{Name: "linear_create_issue"}}, ran: &ran}}
	tb := agent.Join(github, linear)
	p := config.Persona{Nick: "sam"}

	if !agent.MustAsk(tb, p, "linear_create_issue", nil) {
		t.Error("the connectors' toolbox says this call must ask")
	}
	if agent.MustAsk(tb, p, "github_read_file", nil) {
		t.Error("GitHub's toolbox says nothing of the kind")
	}
}

// sourced is a box that says where its tools' content comes from.
type sourced struct {
	box
	source string
}

func (s sourced) SourceOf(string) string { return s.source }

// A write soon after reading from somewhere else asks first, across
// toolboxes too: what a Linear issue says could steer a comment on GitHub,
// and what's in a repo could steer a Linear write. Reading and writing in
// one place doesn't.
func TestAWriteAfterReadingElsewhereAsksFirst(t *testing.T) {
	var ran []string
	github := sourced{box{tools: []agent.Tool{{Name: "github_read_file", ReadOnly: true}, {Name: "github_comment"}}, ran: &ran}, "GitHub"}
	linear := sourced{box{tools: []agent.Tool{{Name: "linear_get_issue", ReadOnly: true}, {Name: "linear_save_issue"}}, ran: &ran}, "Linear"}
	tb := agent.Join(github, linear)
	sam, kai := config.Persona{Nick: "sam"}, config.Persona{Nick: "kai"}

	tb.Run(context.Background(), sam, "github_read_file", nil)
	if agent.MustAsk(tb, sam, "github_comment", nil) {
		t.Error("a GitHub write after a GitHub read asked")
	}
	if !agent.MustAsk(tb, sam, "linear_save_issue", nil) {
		t.Error("a Linear write after a GitHub read didn't ask")
	}
	if ask, why := tb.(agent.Asker).MustAsk(sam, "linear_save_issue", nil); !ask || !strings.Contains(why, "GitHub") {
		t.Errorf("why: %q, want it to say where it read", why)
	}
	if agent.MustAsk(tb, sam, "linear_get_issue", nil) {
		t.Error("a read asked")
	}
	if agent.MustAsk(tb, kai, "linear_save_issue", nil) {
		t.Error("another agent's read counted")
	}

	tb.Run(context.Background(), kai, "linear_get_issue", nil)
	if !agent.MustAsk(tb, kai, "github_comment", nil) {
		t.Error("a GitHub comment after a Linear read didn't ask")
	}
}

// A desk joined to a toolbox that's already joined (GitHub's and the
// connectors') still tells a write to a connector after reading at the desk.
func TestJoiningJoinedToolboxesKeepsTrackOfReads(t *testing.T) {
	var ran []string
	desk := sourced{box{tools: []agent.Tool{{Name: "bash", ReadOnly: true}}, ran: &ran}, "GitHub"}
	github := sourced{box{tools: []agent.Tool{{Name: "github_comment"}}, ran: &ran}, "GitHub"}
	linear := sourced{box{tools: []agent.Tool{{Name: "linear_save_issue"}}, ran: &ran}, "Linear"}
	tb := agent.Join(desk, agent.Join(github, linear))
	sam := config.Persona{Nick: "sam"}

	tb.Run(context.Background(), sam, "bash", nil)

	if !agent.MustAsk(tb, sam, "linear_save_issue", nil) {
		t.Error("a Linear write after work at the desk didn't ask")
	}
	if agent.MustAsk(tb, sam, "github_comment", nil) {
		t.Error("a GitHub comment after work at the desk asked")
	}
}

// What a tool achieved is shown in IRC by its first line; a connector's
// result starts with its <untrusted> tag, which isn't news.
func TestAToolsHeadlineIsItsFirstLineOfNews(t *testing.T) {
	for out, want := range map[string]string{
		"Opened PR #3: fix login\nmore":                              "opened PR #3: fix login",
		"<untrusted source=\"linear\">\nCreated ENG-2\n</untrusted>": "created ENG-2",
		"  \n":                                   "",
		"<untrusted source=\"x\">\n</untrusted>": "",
		// A service's JSON isn't news for people.
		"<untrusted source=\"linear\">\n{\"id\":\"ENG-2\",\"title\":\"x\"}\n</untrusted>": "",
		"<untrusted source=\"linear\">\n[\n  {\"id\": 1}\n]\n</untrusted>":                "",
	} {
		if got := agent.Headline(out); got != want {
			t.Errorf("Headline(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestJoiningNothingIsNoToolbox(t *testing.T) {
	if agent.Join(nil, nil) != nil {
		t.Error("no toolboxes should join to nil")
	}
	var ran []string
	one := &box{ran: &ran}
	if agent.Join(nil, one) != agent.Toolbox(one) {
		t.Error("one toolbox joins to itself")
	}
}
