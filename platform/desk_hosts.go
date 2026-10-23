// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// Hosts a network's desks may reach past the usual ones, on its Connectors
// page: the owner adds and removes them, anyone else asks the owner for one
// (an approval). The list is the runner's, which logs each change and tells
// the broker (news), which emails the owner.

// deskHostView is one host on the list.
type deskHostView struct {
	Host, By, Asked, Why string
	At                   time.Time
	// Blocked: this install's blocklist names it since it was added, so the
	// network's pass doesn't open it.
	Blocked bool
}

// deskHostsView is the Connectors page's section.
type deskHostsView struct {
	List  []deskHostView
	Owner string // who decides
	Asked string // a host just asked for, said once
	Form  string // the host typed, when it wasn't taken
}

// deskHostsOn says whether there are desks of the console's own here: the
// list is for their proxy.
func (p *Platform) deskHostsOn() bool { return p.opts.Sandbox.URL != "" }

// deskHosts is the network's list, as its runner keeps it.
func (p *Platform) deskHosts(r *http.Request) (*deskHostsView, error) {
	var out struct {
		Hosts []struct {
			Host, By, Asked, Why string
			At                   time.Time
		} `json:"hosts"`
		Owner string `json:"owner"`
	}
	if err := p.runner(r).call(r.Context(), "GET", "/v1/desk-hosts", nil, &out); err != nil {
		return nil, err
	}
	v := &deskHostsView{Owner: out.Owner}
	for _, h := range out.Hosts {
		v.List = append(v.List, deskHostView{Host: h.Host, By: h.By, Asked: h.Asked, Why: h.Why, At: h.At, Blocked: sandbox.Blocked(h.Host, p.opts.DeskBlocked)})
	}
	if host := r.URL.Query().Get("asked"); host != "" {
		v.Asked, _ = sandbox.CheckDeskHost(host, nil)
	}
	return v, nil
}

// addDeskHost puts a host on the list: the runner says no to anyone but the
// owner, and to a host desks mustn't reach.
func (p *Platform) addDeskHost(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSpace(r.PostFormValue("host"))
	if problem := p.blockedHere(host); problem != "" {
		p.showDeskHostsAgain(w, r, http.StatusBadRequest, problem, host)
		return
	}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/desk-hosts", map[string]string{"host": host}, nil); err != nil {
		p.showDeskHostsAgain(w, r, statusOf(err), messageOf(err), host)
		return
	}
	http.Redirect(w, r, "/connectors#hosts", http.StatusSeeOther)
}

func (p *Platform) removeDeskHost(w http.ResponseWriter, r *http.Request) {
	if err := p.runner(r).call(r.Context(), "DELETE", "/v1/desk-hosts/"+url.PathEscape(r.PathValue("host")), nil, nil); err != nil {
		p.showConnectors(w, r, statusOf(err), messageOf(err))
		return
	}
	http.Redirect(w, r, "/connectors#hosts", http.StatusSeeOther)
}

// askForDeskHost asks the owner to let desks reach a host, saying why.
func (p *Platform) askForDeskHost(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSpace(r.PostFormValue("host"))
	if problem := p.blockedHere(host); problem != "" {
		p.showDeskHostsAgain(w, r, http.StatusBadRequest, problem, host)
		return
	}
	body := map[string]string{"host": host, "why": r.PostFormValue("why")}
	if err := p.runner(r).call(r.Context(), "POST", "/v1/desk-hosts/ask", body, nil); err != nil {
		p.showDeskHostsAgain(w, r, statusOf(err), messageOf(err), host)
		return
	}
	checked, _ := sandbox.CheckDeskHost(host, nil)
	http.Redirect(w, r, "/connectors?asked="+url.QueryEscape(checked)+"#hosts", http.StatusSeeOther)
}

// blockedHere is why this install's own blocklist (which the runner doesn't
// know) won't have desks reach host, or "".
func (p *Platform) blockedHere(host string) string {
	if h, problem := sandbox.CheckDeskHost(host, nil); problem == "" && sandbox.Blocked(h, p.opts.DeskBlocked) {
		return h + " is blocked here: desks never reach it."
	}
	return ""
}

func (p *Platform) showDeskHostsAgain(w http.ResponseWriter, r *http.Request, status int, problem, host string) {
	p.showConnectorsWith(w, r, status, problem, func(v *connectorsPage) {
		if v.Hosts != nil {
			v.Hosts.Form = host
		}
	})
}

// deskHostEmail tells the owner a host was added to the list, or removed.
func deskHostEmail(n Network, page, host, change, by, asked, why string, at time.Time) Letter {
	said := fmt.Sprintf("%s's desks may reach %s now", n.Name, host)
	what := fmt.Sprintf("%s let %s's desks reach %s, at %s.", by, n.Name, host, stamp(at))
	if asked != "" && why != "" {
		what = fmt.Sprintf("%s let %s's desks reach %s, at %s, as %s asked: %s.", by, n.Name, host, stamp(at), asked, strings.TrimSuffix(why, "."))
	}
	if change == "removed" {
		said = fmt.Sprintf("%s's desks no longer reach %s", n.Name, host)
		what = fmt.Sprintf("%s took %s off the hosts %s's desks may reach, at %s.", by, host, n.Name, stamp(at))
	}
	return Letter{Subject: said, Heading: said,
		Paras:  []string{what, "Anything a desk can read can be sent to a host it reaches: keep the list to hosts you trust.", notYou},
		Button: "See the hosts", Link: page, Why: securityWhy}
}
