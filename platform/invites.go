// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/monitor"
)

// People in a network: the owner invites someone with a one-time link, good
// for a week, that lets them pick a nick and password and join the org as
// the owner set it up. Everyone changes their own password on their account
// page; the owner makes a one-time reset link, good for a day, for someone
// who forgot theirs. An owner who forgot their own sets a new one from the
// home page, signed in with Google (see setOwnerPassword).

const (
	inviteTTL = 7 * 24 * time.Hour
	resetTTL  = 24 * time.Hour
)

// orgHere is the network's org chart as the signed-in person sees it, and
// whether they're its owner.
func (p *Platform) orgHere(r *http.Request) (orgChart, bool, error) {
	var chart orgChart
	if err := p.runner(r).call(r.Context(), "GET", "/v1/org", nil, &chart); err != nil {
		return chart, false, err
	}
	s, _ := sessionOf(r)
	return chart, strings.EqualFold(chart.Owner, s.Account), nil
}

type peopleView struct {
	People []orgPerson
	// Inviting: the teams they may invite into, and whether into no team.
	InviteTeams  []string
	InviteNoTeam bool
	Invites      []Link // waiting to join, that they may cancel
	// Just made, so shown once: the secret isn't kept.
	NewInvite           string
	ResetFor, ResetLink string
	// CanEmail: GuildNet can email invites. Emailed is who this one was
	// emailed to; Unsent who it couldn't be.
	CanEmail        bool
	Emailed, Unsent string
	// Owner: may decide whether guests have agents work for them
	// (GuestsAsk).
	Owner, GuestsAsk bool
}

// CanInvite reports whether they may invite anyone at all.
func (v peopleView) CanInvite() bool { return v.InviteNoTeam || len(v.InviteTeams) > 0 }

type orgPerson struct {
	Name, Role, Team, ReportsTo string
	Guest, Owner                bool
	Access                      []string
	CanReset                    bool
}

// mayReset reports whether m may make reset links: a reset link sets
// someone's IRC password, so whoever makes it could sign in as them. Only
// those who may invite across the whole org (the owner and admins) may.
func (m me) mayReset() bool { return m.inTeam("invite", "") }

func (p *Platform) loadPeople(r *http.Request) (peopleView, error) {
	var chart orgChart
	if err := p.runner(r).call(r.Context(), "GET", "/v1/org", nil, &chart); err != nil {
		return peopleView{}, err
	}
	m, err := p.loadMe(r)
	if err != nil {
		return peopleView{}, err
	}
	var v peopleView
	for _, x := range chart.People {
		if x.Kind != "agent" {
			v.People = append(v.People, orgPerson{Name: x.Name, Role: x.Role, Team: x.Team, ReportsTo: x.ReportsTo,
				Guest: x.Kind == "guest", Owner: x.Owner, Access: x.Access, CanReset: m.mayReset() && !x.Owner && !strings.EqualFold(x.Name, m.Account)})
		}
	}
	v.InviteTeams, v.InviteNoTeam = m.teams("invite", chart.Teams), m.inTeam("invite", "")
	if m.Owner {
		var c company
		if err := p.runner(r).call(r.Context(), "GET", "/v1/company", nil, &c); err != nil {
			return v, err
		}
		v.Owner, v.GuestsAsk = true, c.GuestsAsk
	}
	if v.CanInvite() && p.opts.Links != nil {
		pending, err := p.opts.Links.PendingLinks(r.Context(), networkOf(r).Name, Invite, p.opts.Now())
		if err != nil {
			return v, err
		}
		for _, l := range pending {
			if mayCancel(m, l) {
				v.Invites = append(v.Invites, l)
			}
		}
	}
	return v, nil
}

// setGuestsAsk lets guests have agents work for them, or not. The runner
// refuses anyone but the owner.
func (p *Platform) setGuestsAsk(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"guests_ask": r.PostFormValue("ask") == "on"}
	if err := p.runner(r).call(r.Context(), "PATCH", "/v1/company", body, nil); err != nil {
		p.renderOrgPeople(w, r, statusOf(err), messageOf(err), func(*peopleView) {})
		return
	}
	http.Redirect(w, r, "/people", http.StatusSeeOther)
}

// mayCancel reports whether m may cancel invite l: they made it, or they may
// invite into its team.
func mayCancel(m me, l Link) bool {
	return strings.EqualFold(l.CreatedBy, m.Account) || m.inTeam("invite", l.Team)
}

func (p *Platform) peoplePage(w http.ResponseWriter, r *http.Request) {
	p.renderOrgPeople(w, r, http.StatusOK, "", func(*peopleView) {})
}

func (p *Platform) renderOrgPeople(w http.ResponseWriter, r *http.Request, status int, problem string, with func(*peopleView)) {
	v, err := p.loadPeople(r)
	if err != nil {
		p.render(w, r, statusOf(err), "people", withError{Err: messageOf(err)})
		return
	}
	v.CanEmail = p.opts.Mail != nil
	with(&v)
	p.render(w, r, status, "people", withError{Err: problem, View: v})
}

// mayHere loads what the signed-in person may do and the org chart, and
// checks allowed; if it doesn't hold, the people page says why.
func (p *Platform) mayHere(w http.ResponseWriter, r *http.Request, allowed func(me, orgChart) bool, refusal string) (me, orgChart, bool) {
	var chart orgChart
	m, err := p.loadMe(r)
	if err == nil {
		err = p.runner(r).call(r.Context(), "GET", "/v1/org", nil, &chart)
	}
	if err != nil {
		p.renderOrgPeople(w, r, statusOf(err), messageOf(err), func(*peopleView) {})
		return m, chart, false
	}
	if p.opts.Links == nil || !allowed(m, chart) {
		p.renderOrgPeople(w, r, http.StatusForbidden, refusal, func(*peopleView) {})
		return m, chart, false
	}
	return m, chart, true
}

func (p *Platform) invitePerson(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.PostFormValue("role"))
	if role == "" {
		role = "Team member"
	}
	teamName := strings.TrimSpace(r.PostFormValue("team"))
	_, chart, ok := p.mayHere(w, r, func(m me, _ orgChart) bool { return m.inTeam("invite", teamName) },
		"You can't invite people into "+orNoTeam(teamName)+".")
	if !ok {
		return
	}
	if len(role) > 64 || strings.ContainsAny(role, "\r\n") {
		p.renderOrgPeople(w, r, http.StatusBadRequest, "A role is one line, at most 64 characters.", func(*peopleView) {})
		return
	}
	if teamName != "" && !slices.ContainsFunc(chart.Teams, func(l team) bool { return strings.EqualFold(l.Name, teamName) }) {
		p.renderOrgPeople(w, r, http.StatusBadRequest, "There's no team "+teamName+".", func(*peopleView) {})
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	network := networkOf(r).Name
	if email != "" {
		switch {
		case p.opts.Mail == nil:
			p.renderOrgPeople(w, r, http.StatusBadRequest, p.brand().Name+" can't send email here: make the link and send it yourself.", func(*peopleView) {})
			return
		case !mail.Plausible(email):
			p.renderOrgPeople(w, r, http.StatusBadRequest, "That doesn't look like an email address.", func(*peopleView) {})
			return
		case !p.inviteMails.Try(network, p.opts.Now()):
			p.renderOrgPeople(w, r, http.StatusTooManyRequests, fmt.Sprintf("%s has emailed %d invites today, the most for a day. Make the link and send it yourself.", network, inviteMailsADay), func(*peopleView) {})
			return
		}
	}
	l := Link{Kind: Invite, Role: role, Team: teamName, Guest: r.PostFormValue("guest") == "on"}
	link, err := p.newLink(r, &l, inviteTTL, "/join/")
	if err != nil {
		p.renderOrgPeople(w, r, http.StatusInternalServerError, "Couldn't make the invite. Try again in a moment.", func(*peopleView) {})
		return
	}
	var emailed, unsent string
	if email != "" {
		s, _ := sessionOf(r)
		if err := p.opts.Mail.Send(r.Context(), p.inviteEmail(email, s.Account, network, link, l)); err != nil {
			monitor.Problem(p.opts.Logger, "emailing an invite on %s: %v", network, err)
			unsent = email
		} else {
			emailed = email
		}
	}
	p.renderOrgPeople(w, r, http.StatusOK, "", func(v *peopleView) { v.NewInvite, v.Emailed, v.Unsent = link, emailed, unsent })
}

// inviteMailsADay is how many invites a network may email a day.
const inviteMailsADay = 20

// inviteEmail is the email inviting to into network, from who, with invite
// l's link.
func (p *Platform) inviteEmail(to, who, network, link string, l Link) mail.Message {
	as := ""
	switch {
	case l.Guest:
		as = ", as a guest"
	case l.Team != "":
		as = ", as " + l.Role + " in " + l.Team
	case l.Role != "":
		as = ", as " + l.Role
	}
	return p.message(Letter{To: to, Subject: who + " invited you to " + network + " on " + ourName,
		// Once per link, even if the request is retried.
		Key:     "invite/" + network + "/" + l.ID,
		Heading: who + " invited you to " + network,
		Paras: []string{
			who + " invited you to join " + network + " on " + ourName + as + ".",
			ourName + " is where a company's people and AI agents work together, in chat. You'll choose your own nick and password.",
		},
		Button: "Accept the invite", Link: link,
		After: []string{"The link works once, until " + l.Expires.UTC().Format("Mon 2 Jan 15:04 UTC") + "."},
		Why:   "If you weren't expecting this, you can ignore it: nothing happens unless you use the link.",
	})
}

// orNoTeam names a team for a refusal: "Sales", or "no team".
func orNoTeam(team string) string {
	if team == "" {
		return "no team"
	}
	return team
}

// newLink keeps a one-time link made by the signed-in person, filling in
// l's ID, maker and times, and gives its address.
func (p *Platform) newLink(r *http.Request, l *Link, ttl time.Duration, path string) (string, error) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	token, now := newToken(), p.opts.Now()
	l.ID, l.Network, l.CreatedBy, l.Created, l.Expires = newID(), n.Name, s.Account, now, now.Add(ttl)
	if err := p.opts.Links.AddLink(r.Context(), hashToken(token), *l); err != nil {
		monitor.Problem(p.opts.Logger, "making a %s link on %s: %v", l.Kind, n.Name, err)
		return "", err
	}
	return p.networkOrigin(r, n.Name) + path + token, nil
}

func (p *Platform) cancelInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, _, ok := p.mayHere(w, r, func(me, orgChart) bool { return true }, "")
	if !ok {
		return
	}
	pending, err := p.opts.Links.PendingLinks(r.Context(), networkOf(r).Name, Invite, p.opts.Now())
	if err != nil {
		p.renderOrgPeople(w, r, http.StatusInternalServerError, "Couldn't cancel it. Try again in a moment.", func(*peopleView) {})
		return
	}
	i := slices.IndexFunc(pending, func(l Link) bool { return l.ID == id })
	if i < 0 {
		http.Redirect(w, r, "/people", http.StatusSeeOther)
		return
	}
	if !mayCancel(m, pending[i]) {
		p.renderOrgPeople(w, r, http.StatusForbidden, "You can't cancel that invite.", func(*peopleView) {})
		return
	}
	if err := p.opts.Links.CancelLink(r.Context(), networkOf(r).Name, id, p.opts.Now()); err != nil && !errors.Is(err, ErrNotFound) {
		p.renderOrgPeople(w, r, http.StatusInternalServerError, "Couldn't cancel it. Try again in a moment.", func(*peopleView) {})
		return
	}
	http.Redirect(w, r, "/people", http.StatusSeeOther)
}

func (p *Platform) makeResetLink(w http.ResponseWriter, r *http.Request) {
	_, chart, ok := p.mayHere(w, r, func(m me, _ orgChart) bool { return m.mayReset() },
		"Only the owner and admins make reset links: a reset link lets whoever has it sign in as that person.")
	if !ok {
		return
	}
	account := r.PathValue("account")
	i := slices.IndexFunc(chart.People, func(x chartPerson) bool { return x.Kind != "agent" && strings.EqualFold(x.Name, account) })
	if i < 0 {
		p.renderOrgPeople(w, r, http.StatusNotFound, account+" isn't a person in this org.", func(*peopleView) {})
		return
	}
	// Never the owner's: whoever had it would have their IRC account. The
	// owner sets a new password from the home page.
	if chart.People[i].Owner {
		p.renderOrgPeople(w, r, http.StatusForbidden, chart.People[i].Name+" owns the network: they set a new password from their page on "+p.opts.BaseDomain+".", func(*peopleView) {})
		return
	}
	account = chart.People[i].Name
	link, err := p.newLink(r, &Link{Kind: Reset, Account: account}, resetTTL, "/reset/")
	if err != nil {
		p.renderOrgPeople(w, r, http.StatusInternalServerError, "Couldn't make the link. Try again in a moment.", func(*peopleView) {})
		return
	}
	// Whoever has the link can be them: the owner hears, unless it's theirs.
	n := networkOf(r)
	if s, _ := sessionOf(r); !strings.EqualFold(s.Account, n.OwnerNick) {
		p.tellOwner(r.Context(), n, resetMadeEmail(n, p.networkOrigin(r, n.Name), s.Account, account, p.opts.Now()))
	}
	p.renderOrgPeople(w, r, http.StatusOK, "", func(v *peopleView) { v.ResetFor, v.ResetLink = account, link })
}

// Using a link, on the network's own address, signed in or not.

type linkView struct {
	Link
	Network string
	Nick    string // what they typed, to try again
}

// linkFor finds the link in the path, if it's this network's and of kind.
// If it's no good, the page saying why has been sent.
func (p *Platform) linkFor(w http.ResponseWriter, r *http.Request, kind string) (Link, string, bool) {
	hash := hashToken(r.PathValue("token"))
	l, err := Link{}, ErrNotFound
	if p.opts.Links != nil {
		l, err = p.opts.Links.Link(r.Context(), hash)
	}
	n := networkOf(r)
	if err != nil || l.Kind != kind || !strings.EqualFold(l.Network, n.Name) {
		if err != nil && !errors.Is(err, ErrNotFound) {
			monitor.Problem(p.opts.Logger, "finding a link on %s: %v", n.Name, err)
		}
		http.NotFound(w, r)
		return l, "", false
	}
	what := map[string]string{Invite: "invite", Reset: "link"}[kind]
	switch {
	case !l.Used.IsZero():
		p.render(w, r, http.StatusGone, "link-gone", withError{View: "This " + what + " has already been used. Ask " + l.CreatedBy + " for a new one."})
		return l, "", false
	case !p.opts.Now().Before(l.Expires):
		p.render(w, r, http.StatusGone, "link-gone", withError{View: "This " + what + " has expired. Ask " + l.CreatedBy + " for a new one."})
		return l, "", false
	}
	return l, hash, true
}

func (p *Platform) joinPage(w http.ResponseWriter, r *http.Request) {
	if l, _, ok := p.linkFor(w, r, Invite); ok {
		p.render(w, r, http.StatusOK, "join", withError{View: linkView{Link: l, Network: networkOf(r).Name}})
	}
}

// checkNewPassword says what's wrong with a password chosen on a form.
func checkNewPassword(r *http.Request) string {
	pw := r.PostFormValue("password")
	if msg := passwordProblem(pw); msg != "" {
		return msg
	}
	if pw != r.PostFormValue("confirm") {
		return "The two passwords don't match."
	}
	return ""
}

func (p *Platform) join(w http.ResponseWriter, r *http.Request) {
	l, hash, ok := p.linkFor(w, r, Invite)
	if !ok {
		return
	}
	n := networkOf(r)
	nick := strings.TrimSpace(r.PostFormValue("nick"))
	again := func(status int, msg string) {
		p.render(w, r, status, "join", withError{Err: msg, View: linkView{Link: l, Network: n.Name, Nick: nick}})
	}
	if !config.ValidNick(nick) {
		again(http.StatusBadRequest, "A nick is letters, digits, - and _, starting with a letter, at most 32 characters.")
		return
	}
	if slices.Contains(reservedNicks, strings.ToLower(nick)) {
		again(http.StatusBadRequest, "That nick belongs to one of the network's bots. Pick another.")
		return
	}
	if msg := checkNewPassword(r); msg != "" {
		again(http.StatusBadRequest, msg)
		return
	}
	// Claimed first, so the same invite can't let two people in at once;
	// given back if joining doesn't work out.
	if err := p.opts.Links.UseLink(r.Context(), hash, nick, p.opts.Now()); err != nil {
		p.render(w, r, http.StatusGone, "link-gone", withError{View: "This invite has already been used. Ask " + l.CreatedBy + " for a new one."})
		return
	}
	joined := false
	defer func() {
		if !joined {
			if err := p.opts.Links.ReleaseLink(context.Background(), hash); err != nil {
				monitor.Problem(p.opts.Logger, "giving back an invite on %s: %v", n.Name, err)
			}
		}
	}()
	// Into the org as the owner who invited them, so the org's rules check
	// the nick; then the IRC account, which must be new.
	inviter := runner{n: n, actor: l.CreatedBy}
	member := map[string]any{"account": nick, "role": l.Role, "guest": l.Guest}
	if l.Team != "" {
		member["team"] = l.Team
	}
	if err := inviter.call(r.Context(), "POST", "/v1/members", member, nil); err != nil {
		if s := statusOf(err); s >= 400 && s < 500 {
			again(http.StatusBadRequest, "Pick another nick: "+messageOf(err))
			return
		}
		again(http.StatusBadGateway, messageOf(err))
		return
	}
	if err := p.createPersonsAccount(r.Context(), n, nick, r.PostFormValue("password")); err != nil {
		if err := inviter.call(context.Background(), "DELETE", "/v1/members/"+url.PathEscape(nick), nil, nil); err != nil {
			monitor.Problem(p.opts.Logger, "taking %s back out of %s's org: %v", nick, n.Name, err)
		}
		if errors.Is(err, errAccountExists) {
			again(http.StatusBadRequest, "That nick is taken on "+n.Name+". Pick another.")
			return
		}
		monitor.Problem(p.opts.Logger, "making %s's account on %s: %v", nick, n.Name, err)
		again(http.StatusBadGateway, "The IRC server didn't answer. Try again in a moment.")
		return
	}
	joined = true
	p.opts.Logger.Printf("%s joined %s, invited by %s", nick, n.Name, l.CreatedBy)
	p.tellOwner(r.Context(), n, joinedEmail(n, p.networkOrigin(r, n.Name), nick, l, p.opts.Now()))
	if err := p.signInAs(w, r, n, nick); err != nil {
		monitor.Problem(p.opts.Logger, "signing %s in: %v", nick, err)
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
	}
}

func (p *Platform) resetPage(w http.ResponseWriter, r *http.Request) {
	if l, _, ok := p.linkFor(w, r, Reset); ok {
		p.render(w, r, http.StatusOK, "reset", withError{View: linkView{Link: l, Network: networkOf(r).Name}})
	}
}

func (p *Platform) reset(w http.ResponseWriter, r *http.Request) {
	l, hash, ok := p.linkFor(w, r, Reset)
	if !ok {
		return
	}
	n := networkOf(r)
	if msg := checkNewPassword(r); msg != "" {
		p.render(w, r, http.StatusBadRequest, "reset", withError{Err: msg, View: linkView{Link: l, Network: n.Name}})
		return
	}
	if err := p.opts.Links.UseLink(r.Context(), hash, l.Account, p.opts.Now()); err != nil {
		p.render(w, r, http.StatusGone, "link-gone", withError{View: "This link has already been used. Ask " + l.CreatedBy + " for a new one."})
		return
	}
	if err := p.changePassword(r.Context(), n, l.Account, r.PostFormValue("password"), ""); err != nil {
		p.opts.Links.ReleaseLink(context.Background(), hash)
		p.render(w, r, http.StatusBadGateway, "reset", withError{Err: "Couldn't set it just now. Try again in a moment.", View: linkView{Link: l, Network: n.Name}})
		return
	}
	// The owner's, from the server: they've lost their second step too.
	if l.CreatedBy == ownerResetBy && strings.EqualFold(l.Account, n.OwnerNick) && p.opts.TwoSteps != nil {
		if err := p.resetTwoStep(r.Context(), r, n, l.Account, ownerResetBy+", with reset-owner"); err != nil {
			monitor.Problem(p.opts.Logger, "resetting %s's two-step sign-in on %s: %v", l.Account, n.Name, err)
		}
	}
	if !strings.EqualFold(l.Account, n.OwnerNick) {
		p.tellOwner(r.Context(), n, resetUsedEmail(n, p.networkOrigin(r, n.Name), l.Account, l.CreatedBy, p.opts.Now()))
	}
	if err := p.signInAs(w, r, n, l.Account); err != nil {
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
	}
}

// changePassword sets account's IRC password on n, and signs them out of
// n's console everywhere but the session keep.
func (p *Platform) changePassword(ctx context.Context, n Network, account, password, keep string) error {
	if err := p.keepIRCPassword(ctx, n, account, password); err != nil {
		monitor.Problem(p.opts.Logger, "setting %s's password on %s: %v", account, n.Name, err)
		return err
	}
	p.signOutElsewhere(ctx, n, account, keep)
	p.opts.Logger.Printf("%s's password on %s changed", account, n.Name)
	return nil
}

// Your own account.

type accountView struct {
	Account     string
	Saved       bool
	Alerts      alertView
	AlertsSaved bool
	// Two-step sign-in, if it's offered here.
	TwoStepOffered bool
	TwoStep        twoStepState
}

func (p *Platform) accountPage(w http.ResponseWriter, r *http.Request) {
	p.renderAccount(w, r, http.StatusOK, "", r.URL.Query().Get("saved") == "1")
}

func (p *Platform) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	again := func(status int, msg string) { p.renderAccount(w, r, status, msg, false) }
	res, err := p.checkSignIn(r.Context(), n, s.Account, r.PostFormValue("current"), p.visitor(r))
	switch {
	case err != nil:
		again(http.StatusBadGateway, "Couldn't check that just now. Try again in a moment.")
		return
	case res == signInHeld:
		again(http.StatusTooManyRequests, "Too many wrong passwords. Wait 15 minutes, then try again.")
		return
	case res != signInRight:
		again(http.StatusUnauthorized, "Your current password isn't right.")
		return
	}
	if msg := checkNewPassword(r); msg != "" {
		again(http.StatusBadRequest, msg)
		return
	}
	hash := ""
	if st, ok := r.Context().Value(sessionKey).(sessionWithToken); ok {
		hash = st.hash
	}
	if err := p.changePassword(r.Context(), n, s.Account, r.PostFormValue("password"), hash); err != nil {
		again(http.StatusBadGateway, "Couldn't change it just now. Try again in a moment.")
		return
	}
	http.Redirect(w, r, "/account?saved=1", http.StatusSeeOther)
}

// setOwnerPassword is an owner who forgot their IRC password setting a new
// one from the home page: signing in with Google proves who they are.
func (p *Platform) setOwnerPassword(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	fail := func(status int, msg string) {
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, status, "home-network", withError{Err: msg, View: p.view(r, n)})
	}
	if n.Status != Ready {
		fail(http.StatusBadRequest, n.Name+" isn't running, so its passwords can't be changed now.")
		return
	}
	if msg := checkNewPassword(r); msg != "" {
		fail(http.StatusBadRequest, msg)
		return
	}
	if err := p.changePassword(r.Context(), n, n.OwnerNick, r.PostFormValue("password"), ""); err != nil {
		fail(http.StatusBadGateway, "Couldn't set it just now. Try again in a moment.")
		return
	}
	p.tellOwner(r.Context(), n, passwordEmail(n, p.homeURL(r, "/networks/"+n.Name), homeOf(r).person.Email, p.opts.Now()))
	http.Redirect(w, r, "/networks/"+n.Name+"?password=1", http.StatusSeeOther)
}
