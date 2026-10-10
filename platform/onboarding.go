// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"net/http"
	"slices"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Getting started. A network's owner sees a checklist at the top of the org
// chart until it's done or they hide it; steps GuildNet can see for itself
// tick off by themselves. The extension may add steps of its own.

// A step of the getting-started checklist. The owner can tick off any step
// themselves; some are also done by themselves, when GuildNet sees them done.
type step struct {
	Key, Title, Hint, Link, LinkText string
	Done                             bool
}

var steps = []step{
	{Key: "talk", Title: "Ask one of your agents something", Hint: `In chat, address one by name, like "kit: what can you do?"`, Link: "/chat/", LinkText: "Open chat"},
	{Key: "invite", Title: "Invite a teammate", Hint: "Make them an invite link with their role and team.", Link: "/people", LinkText: "People"},
	{Key: "irc", Title: "Connect an IRC app (optional)", Hint: "Chat from mIRC, your phone, or any IRC app, with the same nick and password.", Link: "/connect", LinkText: "How to connect"},
	{Key: "goals", Title: "Say what the company is for", Hint: "A description and a few goals every agent reads, and each team's focus.", Link: "/company", LinkText: "Company"},
	{Key: "budgets", Title: "Look over teams and budgets", Hint: "What each team may spend, and what its agents and people cost.", Link: "/teams", LinkText: "Teams"},
}

// allSteps are the checklist's steps: the console's, then the extension's.
func (p *Platform) allSteps() []step {
	all := slices.Clone(steps)
	for _, s := range p.hooks.Checklist {
		all = append(all, step{Key: s.Key, Title: s.Title, Hint: s.Hint, Link: s.Link, LinkText: s.LinkText})
	}
	return all
}

// dismissed is the step that hides the whole checklist.
const dismissed = "dismissed"

type checklistView struct {
	Steps []step
	Left  int
}

// checklist is n's getting-started checklist, or nil once it's done or
// hidden. chart is its org chart as its owner sees it.
func (p *Platform) checklist(r *http.Request, chart orgChart) *checklistView {
	if p.opts.Onboarding == nil {
		return nil
	}
	ctx, n := r.Context(), networkOf(r)
	marked, err := p.opts.Onboarding.OnboardingDone(ctx, n.Name)
	if err != nil {
		monitor.Problem(p.opts.Logger, "%s's checklist: %v", n.Name, err)
		return nil
	}
	if marked[dismissed] {
		return nil
	}
	auto := map[string]func() bool{
		"talk": func() bool {
			var s struct {
				SpendMonth float64 `json:"spend_month"`
				CallsToday int     `json:"calls_today"`
			}
			return (runner{n: n}).call(ctx, "GET", "/v1/stats", nil, &s) == nil && (s.SpendMonth > 0 || s.CallsToday > 0)
		},
		"invite": func() bool {
			humans := 0
			for _, x := range chart.People {
				if x.Kind != "agent" {
					humans++
				}
			}
			if humans > 1 || p.opts.Links == nil {
				return humans > 1
			}
			pending, err := p.opts.Links.PendingLinks(ctx, n.Name, Invite, p.opts.Now())
			return err == nil && len(pending) > 0
		},
		"goals": func() bool {
			var c company
			return p.runner(r).call(ctx, "GET", "/v1/company", nil, &c) == nil && (c.Description != "" || len(c.Goals) > 0)
		},
	}
	for _, s := range p.hooks.Checklist {
		if s.Done != nil {
			auto[s.Key] = func() bool { return s.Done(r, n) }
		}
	}
	v := &checklistView{}
	for _, s := range p.allSteps() {
		s.Done = marked[s.Key]
		if check, ok := auto[s.Key]; ok && !s.Done {
			s.Done = check()
		}
		if !s.Done {
			v.Left++
		}
		v.Steps = append(v.Steps, s)
	}
	if v.Left == 0 {
		return nil
	}
	return v
}

// tickOff is the owner ticking off a step, or hiding the checklist.
func (p *Platform) tickOff(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("step")
	if key != dismissed && !slices.ContainsFunc(p.allSteps(), func(s step) bool { return s.Key == key }) {
		http.NotFound(w, r)
		return
	}
	if _, owner, err := p.orgHere(r); err != nil || !owner || p.opts.Onboarding == nil {
		http.Error(w, "Only the owner can do that.", http.StatusForbidden)
		return
	}
	if err := p.opts.Onboarding.MarkOnboarding(r.Context(), networkOf(r).Name, key, p.opts.Now()); err != nil {
		monitor.Problem(p.opts.Logger, "ticking off %s on %s: %v", key, networkOf(r).Name, err)
		http.Error(w, "Couldn't save that. Try again in a moment.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
