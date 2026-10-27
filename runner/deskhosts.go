// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/internal/workweek"
)

// Hosts desks may reach, past those every desk reaches (sandbox.
// DefaultAllowed): the network's owner chooses them, by name. Anyone else
// asks, a person in the console or an agent at its desk (request_host), and
// that goes to the owner, and only the owner, as an approval; approving
// adds the host. The network's egress pass opens them at the desks' proxy.
// Each change is in the org log, and the platform is told (it emails the
// owner).

// DeskHost is a host the network's desks may reach.
type DeskHost struct {
	Host  string    `json:"host"`
	By    string    `json:"by"`              // who added it: the owner
	Asked string    `json:"asked,omitempty"` // who asked for it, if someone else did
	Why   string    `json:"why,omitempty"`
	At    time.Time `json:"at"`
}

const (
	deskHostsSetting = "desk_hosts"
	// maxDeskHosts is how many a network may have.
	maxDeskHosts = 50
	// deskHostTool is the tool an agent asks for a host with, and what a
	// person's asking is raised as.
	deskHostTool = "request_host"
	maxWhy       = 300
)

// DeskHostEvent says a host was added to the network's list, or removed.
type DeskHostEvent struct {
	Host   string `json:"host"`
	Change string `json:"change"` // "added" or "removed"
	By     string `json:"by"`
	Asked  string `json:"asked,omitempty"`
	Why    string `json:"why,omitempty"`
}

// UseDeskHosts has h (the desks' tool to ask for a host, and the hosts for
// the network's pass) answer from this runner's list. deskBlocked are the
// install's own hosts never to reach, beside sandbox.DefaultBlocked.
func (r *Runner) UseDeskHosts(h *DeskHosts, deskBlocked ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.r = r
	r.mu.Lock()
	r.deskBlocked = deskBlocked
	r.mu.Unlock()
}

func (r *Runner) blockedHosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deskBlocked
}

// deskHosts is the network's list.
func (r *Runner) deskHosts() ([]DeskHost, error) {
	v, err := r.store.Setting(context.Background(), deskHostsSetting)
	if err != nil || v == "" {
		return nil, err
	}
	var hosts []DeskHost
	if err := json.Unmarshal([]byte(v), &hosts); err != nil {
		return nil, fmt.Errorf("the list of hosts desks may reach: %w", err)
	}
	return hosts, nil
}

func (r *Runner) saveDeskHosts(hosts []DeskHost) error {
	b, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	return r.store.SetSetting(context.Background(), deskHostsSetting, string(b))
}

// checkNewHost is raw as a host to add, or why it can't be.
func (r *Runner) checkNewHost(raw string, listed []DeskHost) (string, error) {
	host, problem := sandbox.CheckDeskHost(raw, r.blockedHosts())
	switch {
	case problem != "":
		return "", refuse(invalid, "%s", problem)
	case everyDeskReaches(host):
		return "", refuse(invalid, "every desk already reaches %s.", host)
	case slices.ContainsFunc(listed, func(h DeskHost) bool { return h.Host == host }):
		return "", refuse(invalid, "%s is already on the list.", host)
	case len(listed) >= maxDeskHosts:
		return "", refuse(invalid, "the list has %d hosts, as many as it may: remove one first.", maxDeskHosts)
	}
	return host, nil
}

// everyDeskReaches says whether host is one every desk reaches anyway.
func everyDeskReaches(host string) bool {
	for _, a := range sandbox.DefaultAllowed {
		if suffix, ok := strings.CutPrefix(a, "*."); ok && strings.HasSuffix(host, "."+suffix) || host == a {
			return true
		}
	}
	return false
}

// addDeskHost puts raw on the list, as actor (the owner), for asked (who
// asked, if someone else did) and why.
func (r *Runner) addDeskHost(actor, raw, asked, why string) (DeskHost, error) {
	if !r.isOwner(actor) {
		return DeskHost{}, refuse(forbidden, "only %s (the owner) chooses the hosts desks may reach: ask for one instead.", r.cfg.Owner)
	}
	r.deskHostsMu.Lock()
	defer r.deskHostsMu.Unlock()
	hosts, err := r.deskHosts()
	if err != nil {
		return DeskHost{}, r.storeRefusal("the list of hosts", err)
	}
	host, err := r.checkNewHost(raw, hosts)
	if err != nil {
		return DeskHost{}, err
	}
	h := DeskHost{Host: host, By: actor, Asked: asked, Why: why, At: r.now()}
	if err := r.saveDeskHosts(append(hosts, h)); err != nil {
		return DeskHost{}, r.storeRefusal("the list of hosts", err)
	}
	detail := ""
	if asked != "" {
		detail = "asked by " + asked
		if why != "" {
			detail += ": " + why
		}
	}
	r.logChange(actor, "allow desk host", host, "", detail)
	r.tellNews(Event{Type: "desk_host", DeskHost: &DeskHostEvent{Host: host, Change: "added", By: actor, Asked: asked, Why: why}})
	return h, nil
}

// removeDeskHost takes host off the list, as actor (the owner).
func (r *Runner) removeDeskHost(actor, host string) error {
	if !r.isOwner(actor) {
		return refuse(forbidden, "only %s (the owner) chooses the hosts desks may reach.", r.cfg.Owner)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	r.deskHostsMu.Lock()
	defer r.deskHostsMu.Unlock()
	hosts, err := r.deskHosts()
	if err != nil {
		return r.storeRefusal("the list of hosts", err)
	}
	i := slices.IndexFunc(hosts, func(h DeskHost) bool { return h.Host == host })
	if i < 0 {
		return refuse(notFound, "%s isn't on the list.", host)
	}
	if err := r.saveDeskHosts(slices.Delete(hosts, i, i+1)); err != nil {
		return r.storeRefusal("the list of hosts", err)
	}
	r.logChange(actor, "remove desk host", host, "", "")
	r.tellNews(Event{Type: "desk_host", DeskHost: &DeskHostEvent{Host: host, Change: "removed", By: actor}})
	return nil
}

// hostSummary is what an approval for host says it's for; hostAsked reads
// it back (it's made only here, from a host already checked).
func hostSummary(host, why string) string { return "let desks reach " + host + " (" + why + ")" }

func hostAsked(summary string) (host, why string) {
	rest, ok := strings.CutPrefix(summary, "let desks reach ")
	if !ok {
		return "", ""
	}
	host, why, _ = strings.Cut(rest, " (")
	return host, strings.TrimSuffix(why, ")")
}

// checkWhy is why someone wants a host, tidied, or what's wrong with it.
func checkWhy(why string) (string, error) {
	why = strings.Join(strings.Fields(why), " ")
	switch {
	case why == "":
		return "", refuse(invalid, "say why desks need to reach it: the owner decides on that.")
	case len(why) > maxWhy:
		return "", refuse(invalid, "say why in under %d characters.", maxWhy)
	}
	return why, nil
}

// askForHost has person ask the owner to let desks reach raw, for why.
func (r *Runner) askForHost(person, raw, why string) (org.Approval, error) {
	switch {
	case r.isOwner(person):
		return org.Approval{}, refuse(invalid, "you're the owner: add it to the list yourself.")
	case !r.inOrgAsPerson(person):
		return org.Approval{}, refuse(forbidden, "only people in the company can ask for a host.")
	}
	hosts, err := r.deskHosts()
	if err != nil {
		return org.Approval{}, r.storeRefusal("the list of hosts", err)
	}
	host, err := r.checkNewHost(raw, hosts)
	if err != nil {
		return org.Approval{}, err
	}
	if why, err = checkWhy(why); err != nil {
		return org.Approval{}, err
	}
	now := r.now()
	a, err := r.store.RaiseApproval(context.Background(), org.Approval{Agent: person, Approver: r.cfg.Owner, Route: []string{r.cfg.Owner},
		Tool: deskHostTool, Summary: hostSummary(host, why), Asker: person, RaisedAt: now, ExpiresAt: workweek.After(now, approvalWindow, r.loc())})
	if err != nil {
		return a, r.storeRefusal("the request", err)
	}
	r.guildConn.Privmsg(a.Approver, truncate(fmt.Sprintf("#%d: %s %s. Reply APPROVE %d, or DENY %d <reason>. Expires %s.",
		a.ID, person, wants(a, shortSummary(a.Summary)), a.ID, a.ID, a.ExpiresAt.In(r.loc()).Format("Mon 15:04 MST")), maxLineBytes))
	r.logger.Printf("approval #%d: %s %s; asked %s", a.ID, person, wants(a, a.Summary), a.Approver)
	r.tellNews(approvalEvent(a, "the console", ""))
	return a, nil
}

// hostDecided adds the host an approval asked for, once the owner approved
// it, and tells a person who asked how it went (an agent hears as it
// carries on).
func (r *Runner) hostDecided(actor string, a org.Approval, allow bool, reason string) {
	host, why := hostAsked(a.Summary)
	_, agentAsked := r.persona(a.Agent)
	var err error
	if allow {
		_, err = r.addDeskHost(actor, host, a.Agent, why)
		if err != nil && !strings.Contains(err.Error(), "already") {
			r.logger.Printf("approval #%d: adding %s: %v", a.ID, host, err)
		}
	}
	if agentAsked {
		return
	}
	switch {
	case !allow && reason != "":
		r.guildReply(a.Agent, fmt.Sprintf("#%d: %s didn't let desks reach %s: %s", a.ID, actor, host, reason))
	case !allow:
		r.guildReply(a.Agent, fmt.Sprintf("#%d: %s didn't let desks reach %s.", a.ID, actor, host))
	case err != nil && !strings.Contains(err.Error(), "already"):
		r.guildReply(a.Agent, fmt.Sprintf("#%d: %s approved, but %s couldn't be added: %v", a.ID, actor, host, err))
	default:
		r.guildReply(a.Agent, fmt.Sprintf("#%d: %s let desks reach %s.", a.ID, actor, host))
	}
}

// hostExpired tells a person who asked for a host that nobody decided (an
// agent hears as it carries on).
func (r *Runner) hostExpired(a org.Approval) {
	if _, agentAsked := r.persona(a.Agent); agentAsked {
		return
	}
	host, _ := hostAsked(a.Summary)
	r.guildReply(a.Agent, fmt.Sprintf("#%d expired: %s didn't decide within 24 working hours, so desks can't reach %s. Ask again if they still need it.", a.ID, a.Approver, host))
}

// The runner's API: the list for anyone in the company; the owner changes
// it, anyone else asks.

func (r *Runner) apiDeskHosts(w http.ResponseWriter, req *http.Request) {
	hosts, err := r.deskHosts()
	if err != nil {
		apiRefusal(w, r.storeRefusal("the list of hosts", err))
		return
	}
	if hosts == nil {
		hosts = []DeskHost{}
	}
	apiJSON(w, http.StatusOK, map[string]any{"hosts": hosts, "owner": r.cfg.Owner})
}

func (r *Runner) apiAddDeskHost(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Host string `json:"host"`
	}
	if !decode(w, req, &in) {
		return
	}
	h, err := r.addDeskHost(actorOf(req), in.Host, "", "")
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, h)
}

func (r *Runner) apiRemoveDeskHost(w http.ResponseWriter, req *http.Request) {
	if err := r.removeDeskHost(actorOf(req), req.PathValue("host")); err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]any{"removed": req.PathValue("host")})
}

func (r *Runner) apiAskForDeskHost(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Host string `json:"host"`
		Why  string `json:"why"`
	}
	if !decode(w, req, &in) {
		return
	}
	a, err := r.askForHost(actorOf(req), in.Host, in.Why)
	if err != nil {
		apiRefusal(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]any{"approval": a.ID, "approver": a.Approver})
}

// DeskHosts is what agents at desks of GuildNet's own have of the list: a
// tool to ask for a host their desk can't reach (request_host, which always
// asks the owner), and the hosts, for the network's egress pass. It answers
// from a runner once UseDeskHosts is called; before, from no list at all.
type DeskHosts struct {
	mu sync.Mutex
	r  *Runner
}

func (h *DeskHosts) runner() *Runner {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.r
}

// Hosts are the names on the network's list.
func (h *DeskHosts) Hosts() []string {
	r := h.runner()
	if r == nil {
		return nil
	}
	hosts, err := r.deskHosts()
	if err != nil {
		return nil
	}
	var out []string
	for _, x := range hosts {
		out = append(out, x.Host)
	}
	return out
}

// Tools: the one, for agents at desks of GuildNet's own (whose way out is
// the desks' proxy).
func (h *DeskHosts) Tools(p config.Persona) []agent.Tool {
	if p.Platform != PlatformDesk {
		return nil
	}
	return []agent.Tool{{Name: deskHostTool, Ask: true,
		Description: "Ask the network's owner to let desks reach a host your desk can't (the proxy refused it: \"isn't on this desk's list of allowed hosts\"). Once they approve, every desk here reaches it, over HTTPS, from your next command.",
		Properties: map[string]any{
			"host": map[string]any{"type": "string", "description": "The host's name, like api.example.com: no https://, port or path"},
			"why":  map[string]any{"type": "string", "description": "Why you need it, for the owner, in a sentence"},
		},
		Required: []string{"host", "why"}}}
}

// Guide tells an agent at a desk when to ask.
func (h *DeskHosts) Guide(p config.Persona) string {
	if p.Platform != PlatformDesk {
		return ""
	}
	return "Your desk reaches GitHub, the Go, npm and PyPI registries, and the hosts the network's owner allowed. If a command is refused a host (\"isn't on this desk's list of allowed hosts\"), and you need it for the work, ask for it with request_host, saying why; don't try another way round."
}

// Prepare checks the host can be asked for, and pins it.
func (h *DeskHosts) Prepare(_ context.Context, p config.Persona, name string, in map[string]any) (string, map[string]any, error) {
	if name != deskHostTool {
		return "", nil, fmt.Errorf("there's no tool called %s", name)
	}
	raw, _ := in["host"].(string)
	why, _ := in["why"].(string)
	var listed []DeskHost
	var blocked []string
	if r := h.runner(); r != nil {
		var err error
		if listed, err = r.deskHosts(); err != nil {
			return "", nil, err
		}
		blocked = r.blockedHosts()
	}
	host, problem := sandbox.CheckDeskHost(raw, blocked)
	switch {
	case problem != "":
		return "", nil, errors.New(problem)
	case everyDeskReaches(host) || slices.ContainsFunc(listed, func(x DeskHost) bool { return x.Host == host }):
		return "", nil, fmt.Errorf("your desk already reaches %s: if a command is still refused, check the name it uses", host)
	}
	why, err := checkWhy(why)
	if err != nil {
		return "", nil, err
	}
	return hostSummary(host, why), map[string]any{"host": host, "why": why}, nil
}

// Run is the tool once the owner approved: the host was added then.
func (h *DeskHosts) Run(_ context.Context, p config.Persona, name string, in map[string]any) (string, error) {
	if name != deskHostTool {
		return "", fmt.Errorf("there's no tool called %s", name)
	}
	host, _ := in["host"].(string)
	if slices.Contains(h.Hosts(), host) {
		return fmt.Sprintf("Desks may reach %s now: run the command again.", host), nil
	}
	return "", fmt.Errorf("%s isn't on the list of hosts desks may reach", host)
}
