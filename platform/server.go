// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// The Server page: a self-hosted install's own, in its network's console.
// What's running, whether each part answers, what's set up and what isn't,
// and what to back up. Its owner uses it; whoever may change things across
// the whole org (who must use two-step sign-in) sees it. Networks GuildNet
// hosts have none: their install is GuildNet's.

type serverView struct {
	Owner   bool // may use it, not only see it
	Version string
	Parts   []serverPart // what's running, and whether it answers
	SetUp   []serverPart // what's set up
	Sent    bool         // a test email just went
	Email   string       // where a test email goes: the owner's confirmed address
}

// serverPart is one thing the page says: what it is, how it is, and
// whether that's good; Docs is the self-hosting page about it.
type serverPart struct {
	Name, State, Docs string
	OK                bool
}

// serverAccess says whether the signed-in person may see the Server page,
// and use it.
func (p *Platform) serverAccess(r *http.Request) (see, use bool) {
	if p.opts.FirstRun == nil {
		return false, false
	}
	s, _ := sessionOf(r)
	n := networkOf(r)
	if n.OwnerNick != "" && strings.EqualFold(s.Account, n.OwnerNick) {
		return true, true
	}
	m, err := p.loadMe(r)
	return err == nil && mustUseTwoStep(m), false
}

func (p *Platform) serverPage(w http.ResponseWriter, r *http.Request) {
	see, use := p.serverAccess(r)
	if !see {
		p.notFound(w, r)
		return
	}
	p.renderServer(w, r, http.StatusOK, use, "")
}

func (p *Platform) renderServer(w http.ResponseWriter, r *http.Request, status int, use bool, problem string) {
	n := networkOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	v := serverView{Owner: use, Version: monitor.FromEnv("platform").Release, Sent: r.URL.Query().Get("sent") == "1"}
	if v.Version == "" {
		v.Version = "a development build"
	}
	running := func(err error) (string, bool) {
		if err != nil {
			return "Not answering", false
		}
		return "Running", true
	}
	stats, ergoErr := ergoStatus(ctx, n)
	irc, ircOK := running(ergoErr)
	if ircOK {
		irc += fmt.Sprintf(" · %d connected · %d channels", stats.Users.Total, stats.Channels)
	}
	_, meErr := p.loadMe(r)
	run, runOK := running(meErr)
	v.Parts = []serverPart{
		{Name: "Console and its database", State: "Running", OK: true},
		{Name: "IRC server", State: irc, OK: ircOK},
		{Name: "Runner (the agents)", State: run, OK: runOK},
	}
	if p.opts.Sandbox.URL != "" {
		until, err := p.sandboxdCertificate(ctx)
		desks, desksOK := running(err)
		if !until.IsZero() {
			desks += " · certificate until " + until.UTC().Format("2 January 2006")
			if until.Sub(p.opts.Now()) < sandboxCertWarn {
				desks, desksOK = desks+": it runs out soon, issue it a new one (docs/self-host/desks-host.md)", false
			}
		}
		v.Parts = append(v.Parts, serverPart{Name: "Desks (sandboxd)", State: desks, OK: desksOK})
	}
	on := func(b bool, how string) (string, bool) {
		if b {
			return how, true
		}
		return "Off", false
	}
	email, emailOK := on(p.opts.Mail != nil, "On")
	github, githubOK := on(p.githubApp != nil, "On, as "+p.githubSlug())
	connectors, connectorsOK := on(p.opts.Connections != nil, "On")
	desks, desksOK := on(p.opts.Sandbox.URL != "", "On")
	// They're on for agents once the runner has them: it asks for the
	// sandbox every minute until it does.
	if desksOK && !p.desksHere(r).Own {
		desks, desksOK = "On, waiting for the runner to take them up (it checks every minute)", false
	}
	https, httpsOK := on(p.opts.SecureCookies, "On")
	irc, ircOn := on(p.opts.IRCPort != 0, "On, port "+strconv.Itoa(p.opts.IRCPort))
	v.SetUp = []serverPart{
		{Name: "Email", State: email, OK: emailOK, Docs: "email.md"},
		{Name: "GitHub", State: github, OK: githubOK, Docs: "github.md"},
		{Name: "Connectors", State: connectors, OK: connectorsOK, Docs: "connectors.md"},
		{Name: "Desks (agents' workspaces)", State: desks, OK: desksOK},
		{Name: "HTTPS and secure sign-in", State: https, OK: httpsOK},
		{Name: "IRC apps", State: irc, OK: ircOn},
	}
	if use && p.opts.Alerts != nil {
		a, err := p.opts.Alerts.AlertSettings(ctx, n.Name, n.OwnerNick)
		if err != nil {
			monitor.Problem(p.opts.Logger, "loading %s's alerts on %s: %v", n.OwnerNick, n.Name, err)
		}
		v.Email = a.Email
	}
	p.render(w, r, status, "server", withError{Err: problem, View: v})
}

// githubSlug is the GitHub App's name, if there's one.
func (p *Platform) githubSlug() string {
	if p.opts.GitHub == nil {
		return ""
	}
	return p.opts.GitHub.Slug
}

// sendTestEmail emails the owner, at their confirmed address, to show
// email works; if it doesn't, the page says what the mail server said.
func (p *Platform) sendTestEmail(w http.ResponseWriter, r *http.Request) {
	if _, use := p.serverAccess(r); !use {
		p.notFound(w, r)
		return
	}
	n := networkOf(r)
	if p.opts.Mail == nil || p.opts.Alerts == nil {
		p.renderServer(w, r, http.StatusBadRequest, true, "Email isn't set up, so there's nothing to test.")
		return
	}
	a, err := p.opts.Alerts.AlertSettings(r.Context(), n.Name, n.OwnerNick)
	if err != nil || a.Email == "" {
		p.renderServer(w, r, http.StatusBadRequest, true, "Confirm an address for your alerts on your account page first: the test email goes there.")
		return
	}
	origin := p.networkOrigin(r, n.Name)
	l := Letter{To: a.Email, Subject: "A test email from " + n.Name,
		Heading: "Email works",
		Paras:   []string{"This is a test email from " + n.Name + "'s " + ourName + ", sent from its Server page by " + n.OwnerNick + ". Alerts and invitations can go out."},
		Button:  "Open the Server page", Link: origin + "/server",
		Why: "You're getting this because you asked for it on " + n.Name + "'s Server page.",
	}
	if err := p.opts.Mail.Send(r.Context(), p.message(l)); err != nil {
		monitor.Problem(p.opts.Logger, "sending %s's test email: %v", n.Name, err)
		p.renderServer(w, r, http.StatusBadGateway, true, "It didn't go. The mail server said: "+monitor.Scrub(err.Error()))
		return
	}
	http.Redirect(w, r, "/server?sent=1", http.StatusSeeOther)
}
