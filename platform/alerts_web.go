// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Email alerts, on the account page: an address (confirmed by a link sent
// there first; the owner's is their Google account's) and whether to email
// approvals waiting on them, off until they turn it on. Networks' runners
// tell the platform what happened through the broker (POST /events).

const confirmTTL = 24 * time.Hour

// alertView is the alerts part of the account page.
type alertView struct {
	CanEmail  bool   // GuildNet sends email here
	Owner     bool   // their address is their Google account's
	Email     string // confirmed
	Pending   string // waiting for its link
	Sent      bool   // a confirmation link was just sent
	Approvals bool
}

// alertsOf is where account's alerts go on n, and whether approvals do: the
// owner's Google address, or the address they confirmed.
func (p *Platform) alertsOf(ctx context.Context, n Network, account string) (alertView, error) {
	v := alertView{CanEmail: p.opts.Mail != nil && p.opts.Alerts != nil}
	if p.opts.Alerts == nil {
		return v, nil
	}
	s, err := p.opts.Alerts.AlertSettings(ctx, n.Name, account)
	if err != nil {
		return v, err
	}
	v.Email, v.Pending, v.Approvals = s.Email, s.Pending, s.Approvals
	if n.Owner != "" && strings.EqualFold(account, n.OwnerNick) && p.opts.People != nil {
		owner, err := p.opts.People.Person(ctx, n.Owner)
		if err != nil {
			return v, err
		}
		v.Owner, v.Email, v.Pending = true, owner.Email, ""
	}
	return v, nil
}

func (p *Platform) renderAccount(w http.ResponseWriter, r *http.Request, status int, problem string, saved bool) {
	p.renderAccountWith(w, r, status, problem, func(v *accountView) { v.Saved = saved })
}

// renderAccountWith is the account page, with what more set shows.
func (p *Platform) renderAccountWith(w http.ResponseWriter, r *http.Request, status int, problem string, more func(*accountView)) {
	s, _ := sessionOf(r)
	v := accountView{Account: s.Account}
	more(&v)
	apps, err := p.passwords().AppPasswords(r.Context(), networkOf(r).Name, s.Account)
	if err != nil {
		monitor.Problem(p.opts.Logger, "loading %s's app passwords on %s: %v", s.Account, networkOf(r).Name, err)
	}
	v.AppPasswords = apps
	alerts, err := p.alertsOf(r.Context(), networkOf(r), s.Account)
	if err != nil {
		monitor.Problem(p.opts.Logger, "loading %s's alerts on %s: %v", s.Account, networkOf(r).Name, err)
	}
	alerts.Sent = r.URL.Query().Get("sent") == "1"
	v.Alerts, v.AlertsSaved = alerts, r.URL.Query().Get("alerts") == "1"
	v.TwoStepOffered = p.opts.TwoSteps != nil
	v.TwoStep = p.twoStepStateOf(r)
	p.render(w, r, status, "account", withError{Err: problem, View: v})
}

// giveAlertAddress keeps an address to confirm, and sends it the link.
func (p *Platform) giveAlertAddress(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	v, err := p.alertsOf(r.Context(), n, s.Account)
	switch {
	case err != nil:
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't load your alerts. Try again in a moment.", false)
		return
	case !v.CanEmail:
		p.renderAccount(w, r, http.StatusBadRequest, p.brand().Name+" can't send email here.", false)
		return
	case v.Owner:
		p.renderAccount(w, r, http.StatusBadRequest, "As the owner, your alerts go to the address you sign in to "+p.brand().Name+" with.", false)
		return
	case !mail.Plausible(email):
		p.renderAccount(w, r, http.StatusBadRequest, "That doesn't look like an email address.", false)
		return
	case !p.inviteMails.Try(n.Name, p.opts.Now()):
		p.renderAccount(w, r, http.StatusTooManyRequests, fmt.Sprintf("%s has sent %d emails to new addresses today, the most for a day. Try again tomorrow.", n.Name, inviteMailsADay), false)
		return
	}
	token := newToken()
	if err := p.opts.Alerts.AwaitConfirmation(r.Context(), n.Name, s.Account, email, hashToken(token), p.opts.Now().Add(confirmTTL)); err != nil {
		monitor.Problem(p.opts.Logger, "keeping %s's alert address on %s: %v", s.Account, n.Name, err)
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't save that. Try again in a moment.", false)
		return
	}
	if err := p.opts.Mail.Send(r.Context(), p.message(confirmEmail(email, s.Account, n.Name, p.networkOrigin(r, n.Name)+"/account/email/confirm/"+token))); err != nil {
		monitor.Problem(p.opts.Logger, "emailing %s's confirmation link on %s: %v", s.Account, n.Name, err)
		p.renderAccount(w, r, http.StatusBadGateway, "Couldn't send the confirmation email just now. Try again in a moment.", false)
		return
	}
	http.Redirect(w, r, "/account?sent=1", http.StatusSeeOther)
}

// confirmAlertAddress is the link in the confirmation email: whoever opens
// it has the mailbox, so no sign-in is needed.
func (p *Platform) confirmAlertAddress(w http.ResponseWriter, r *http.Request) {
	n := networkOf(r)
	if p.opts.Alerts == nil {
		http.NotFound(w, r)
		return
	}
	network, account, email, err := p.opts.Alerts.Confirm(r.Context(), hashToken(r.PathValue("token")), p.opts.Now())
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrLinkUsed), err == nil && !strings.EqualFold(network, n.Name):
		p.render(w, r, http.StatusGone, "link-gone", withError{View: "This link has been used or has expired. Give your address again on your account page for a new one."})
	case err != nil:
		monitor.Problem(p.opts.Logger, "confirming an alert address on %s: %v", n.Name, err)
		http.Error(w, "Something went wrong. Try again in a moment.", http.StatusInternalServerError)
	default:
		p.opts.Logger.Printf("%s confirmed an alert address on %s", account, n.Name)
		p.render(w, r, http.StatusOK, "link-gone", withError{View: "Your address is confirmed: " + n.Name + "'s alerts for " + account + " can go to " + email + ". Choose which on your account page."})
	}
}

// setAlerts turns approval emails on or off.
func (p *Platform) setAlerts(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	on := r.PostFormValue("approvals") == "on"
	v, err := p.alertsOf(r.Context(), n, s.Account)
	switch {
	case err != nil:
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't load your alerts. Try again in a moment.", false)
		return
	case !v.CanEmail:
		p.renderAccount(w, r, http.StatusBadRequest, p.brand().Name+" can't send email here.", false)
		return
	case on && v.Email == "":
		p.renderAccount(w, r, http.StatusBadRequest, "Confirm your address first: approval emails only go to an address you've confirmed.", false)
		return
	}
	if err := p.opts.Alerts.SetApprovalAlerts(r.Context(), n.Name, s.Account, on); err != nil {
		monitor.Problem(p.opts.Logger, "setting %s's alerts on %s: %v", s.Account, n.Name, err)
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't save that. Try again in a moment.", false)
		return
	}
	http.Redirect(w, r, "/account?alerts=1", http.StatusSeeOther)
}

// The broker's news from runners.

type newsEvent struct {
	Type     string `json:"type"`
	Approval *struct {
		ID        int64     `json:"id"`
		Agent     string    `json:"agent"`
		Summary   string    `json:"summary"`
		Where     string    `json:"where"`
		Asker     string    `json:"asker"`
		Approver  string    `json:"approver"`
		MovedFrom string    `json:"moved_from"`
		Expires   time.Time `json:"expires"`
		OwnWords  bool      `json:"own_words"`
	} `json:"approval"`
	Spend *struct {
		Spent float64 `json:"spent"`
		Cap   float64 `json:"cap"`
	} `json:"spend"`
	DeskHost *struct {
		Host   string `json:"host"`
		Change string `json:"change"`
		By     string `json:"by"`
		Asked  string `json:"asked"`
		Why    string `json:"why"`
	} `json:"desk_host"`
}

// news takes what a network's runner says happened, and emails whoever
// wants to hear it.
func (p *Platform) news(w http.ResponseWriter, r *http.Request) {
	n, ok := p.runnerCalling(w, r)
	if !ok {
		return
	}
	var e newsEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&e); err != nil {
		brokerError(w, http.StatusBadRequest, "that isn't an event")
		return
	}
	origin := p.scheme() + "://" + n.Name + "." + p.opts.BaseDomain
	switch {
	case e.Type == "spend_cap" && e.Spend != nil:
		p.tellOwner(r.Context(), n, capEmail(n, origin, e.Spend.Spent, e.Spend.Cap, p.opts.Now()))
	case e.Type == "desk_host" && e.DeskHost != nil && e.DeskHost.Host != "":
		h := e.DeskHost
		p.tellOwner(r.Context(), n, deskHostEmail(n, origin+"/connectors#hosts", oneLine(h.Host, 253), h.Change, oneLine(h.By, 64), oneLine(h.Asked, 64), oneLine(h.Why, 300), p.opts.Now()))
	case e.Type == "approval" && e.Approval != nil && config.ValidNick(e.Approval.Approver):
		a := e.Approval
		v, err := p.alertsOf(r.Context(), n, a.Approver)
		if err != nil {
			monitor.Problem(p.opts.Logger, "loading %s's alerts on %s: %v", a.Approver, n.Name, err)
		}
		if v.CanEmail && v.Approvals && v.Email != "" {
			l := approvalEmail(n, origin, a.ID, a.Agent, a.Summary, a.Where, a.Asker, a.MovedFrom, a.Expires, a.OwnWords)
			l.To = v.Email
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
			defer cancel()
			if err := p.opts.Mail.Send(ctx, p.message(l)); err != nil {
				monitor.Problem(p.opts.Logger, "emailing %s approval #%d on %s: %v", a.Approver, a.ID, n.Name, err)
			}
		}
	default:
		brokerError(w, http.StatusBadRequest, "unknown event")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
