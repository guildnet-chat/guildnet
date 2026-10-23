// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
	"github.com/guildnet-chat/guildnet/netspec"
)

// A network's settings, for its owner, in its console: the company
// timezone, the model providers' keys its agents run on, and deleting it.
// Only for networks with an owner, run by the host (created on the home
// page, or by first-run setup); one set up by hand is changed by hand.

type settingsView struct {
	Name, Timezone string
	Timezones      []string
	Providers      []providerKey
	Saved          bool
	// CanDelete: it's deleted here, not with the install (FirstRun).
	CanDelete bool
	// Sections are the extension's (GuildNet's plan and desk hours).
	Sections []template.HTML
}

// providerKey is a model provider as settings show it: never the key, only
// its end.
type providerKey struct {
	Provider, Title, KeyEnd string
}

// ownedHere is the console's network, if the host runs it and the
// signed-in person is its owner.
func (p *Platform) ownedHere(w http.ResponseWriter, r *http.Request) (Network, bool) {
	n := networkOf(r)
	if n.OwnerNick == "" || p.opts.Host == nil {
		http.NotFound(w, r)
		return Network{}, false
	}
	if s, _ := sessionOf(r); !strings.EqualFold(s.Account, n.OwnerNick) {
		http.Error(w, "Only "+n.Name+"'s owner can change its settings.", http.StatusForbidden)
		return Network{}, false
	}
	return n, true
}

// homeOrigin is the home page's address, with the port this request came
// in on.
func (p *Platform) homeOrigin(r *http.Request) string {
	origin := p.scheme() + "://" + p.opts.BaseDomain
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		origin += ":" + port
	}
	return origin
}

func (p *Platform) settingsPage(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownedHere(w, r)
	if !ok {
		return
	}
	p.renderSettings(w, r, http.StatusOK, n, "")
}

func (p *Platform) renderSettings(w http.ResponseWriter, r *http.Request, status int, n Network, problem string) {
	v := settingsView{Name: n.Name, Timezone: n.Timezone, Timezones: commonTimezones, Saved: r.URL.Query().Get("saved") == "1", CanDelete: p.opts.FirstRun == nil}
	for _, prov := range models.Providers {
		v.Providers = append(v.Providers, providerKey{Provider: string(prov), Title: prov.Title(), KeyEnd: keyEnd(n.Keys.Get(prov))})
	}
	v.Sections = p.sections(r, n)
	p.render(w, r, status, "settings", withError{Err: problem, View: v})
}

// specFor is what the host needs to run n, served at origin: never its
// model keys when its runner fetches them from the broker.
func (p *Platform) specFor(n Network, origin string) netspec.Spec {
	s := netspec.Spec{
		Template: n.Template, Owner: n.OwnerNick, Company: n.Name, Timezone: n.Timezone, WebOrigin: origin,
		RunnerToken: n.RunnerToken, ErgoToken: n.ErgoToken, ChatKey: n.ChatKey,
	}
	if !p.opts.KeysByBroker {
		s.ModelKeys = n.Keys.Map()
	}
	return s
}

// apply has the host run the network with its new settings (its runner
// restarts with them), then keeps them; it reports whether that worked.
func (p *Platform) apply(w http.ResponseWriter, r *http.Request, n Network) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if _, err := p.opts.Host.Put(ctx, n.Name, p.specFor(n, p.networkOrigin(r, n.Name))); err != nil {
		monitor.Problem(p.opts.Logger, "changing %s's settings: %v", n.Name, err)
		p.renderSettings(w, r, http.StatusBadGateway, networkOf(r), "Couldn't apply that just now. Nothing changed; try again in a moment.")
		return false
	}
	n.Updated = p.opts.Now()
	if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
		monitor.Problem(p.opts.Logger, "saving %s's settings: %v", n.Name, err)
		p.renderSettings(w, r, http.StatusInternalServerError, networkOf(r), "Couldn't save that. Try again in a moment.")
		return false
	}
	// The runner fetches its model keys from the broker; have it fetch them
	// now, so a change works at once (it also does every few minutes).
	if err := p.runner(r).call(ctx, "POST", "/v1/model-keys/refresh", nil, nil); err != nil {
		monitor.Problem(p.opts.Logger, "asking %s's runner to fetch its keys: %v", n.Name, err)
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
	return true
}

func (p *Platform) saveTimezone(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownedHere(w, r)
	if !ok {
		return
	}
	tz := strings.TrimSpace(r.PostFormValue("timezone"))
	if loc, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" || loc == nil {
		p.renderSettings(w, r, http.StatusBadRequest, n, "Pick a timezone like America/Edmonton or Europe/Paris.")
		return
	}
	n.Timezone = tz
	p.apply(w, r, n)
}

// saveKey adds a model provider's key, or replaces it.
func (p *Platform) saveKey(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownedHere(w, r)
	if !ok {
		return
	}
	prov, ok := providerNamed(r.PostFormValue("provider"))
	if !ok {
		p.renderSettings(w, r, http.StatusBadRequest, n, "Pick the model provider the key is for.")
		return
	}
	key := strings.TrimSpace(r.PostFormValue("model_key"))
	if msg := p.checkKey(r.Context(), prov, key); msg != "" {
		p.renderSettings(w, r, http.StatusBadRequest, n, msg)
		return
	}
	what := "added"
	if n.Keys.Get(prov) != "" {
		what = "replaced"
	}
	n.Keys.Set(prov, key)
	if p.apply(w, r, n) {
		p.keyChanged(r, n, prov.Title(), what)
	}
}

// keyChanged alerts the owner that one of the network's keys changed.
func (p *Platform) keyChanged(r *http.Request, n Network, provider, what string) {
	s, _ := sessionOf(r)
	p.tellOwner(r.Context(), n, keyEmail(n, p.networkOrigin(r, n.Name), provider, what, s.Account, p.opts.Now()))
}

// removeKey takes a model provider's key away; never the last one.
func (p *Platform) removeKey(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownedHere(w, r)
	if !ok {
		return
	}
	prov, ok := providerNamed(r.PostFormValue("provider"))
	if !ok || n.Keys.Get(prov) == "" {
		p.renderSettings(w, r, http.StatusBadRequest, n, "There's no key for that provider.")
		return
	}
	if len(n.Keys.Map()) == 1 {
		p.renderSettings(w, r, http.StatusBadRequest, n, "Your agents need at least one model provider's key. Add another before removing this one.")
		return
	}
	n.Keys.Set(prov, "")
	if p.apply(w, r, n) {
		p.keyChanged(r, n, prov.Title(), "removed")
	}
}

func (p *Platform) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownedHere(w, r)
	if !ok {
		return
	}
	if p.opts.FirstRun != nil {
		p.renderSettings(w, r, http.StatusBadRequest, n, n.Name+" is this install's own network: it's removed with the install, by stopping its stack and removing its volumes.")
		return
	}
	if r.PostFormValue("confirm") != n.Name {
		p.renderSettings(w, r, http.StatusBadRequest, n, "Type the network's name, "+n.Name+", to delete it.")
		return
	}
	if refusal, err := p.mayDelete(r.Context(), n); err != nil || refusal != "" {
		if err != nil {
			monitor.Problem(p.opts.Logger, "whether %s may be deleted: %v", n.Name, err)
			p.renderSettings(w, r, http.StatusInternalServerError, n, "Couldn't delete it just now. Nothing is lost; try again in a moment.")
			return
		}
		p.renderSettings(w, r, http.StatusBadRequest, n, refusal)
		return
	}
	if err := p.startRemoving(r.Context(), n); err != nil {
		p.renderSettings(w, r, http.StatusInternalServerError, n, "Couldn't delete it just now. Nothing is lost; try again in a moment.")
		return
	}
	s, _ := sessionOf(r)
	p.tellOwner(r.Context(), n, deletedEmail(n, p.homeOrigin(r)+"/networks", s.Account, p.opts.Now()))
	// Signed out (the sign-in goes with the network), and a page here, not
	// a redirect home: the console's forms are boosted, and htmx can't
	// follow a redirect to another address.
	p.endSession(w, r)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, nil))
	p.render(w, r, http.StatusOK, "deleted", deletedView{Name: n.Name, Home: p.homeOrigin(r) + "/networks"})
}

type deletedView struct{ Name, Home string }

// deleteProblem is what a network's owner sees when deleting it failed.
const deleteProblem = "Deleting it didn't finish."

// startRemoving marks the network as being deleted (from then on it serves
// nothing) and has the host take it away in the background. That's not
// done while answering the request: taking the platform off the network's
// private Docker network can drop the connection the request came on.
func (p *Platform) startRemoving(ctx context.Context, n Network) error {
	n.Status, n.Problem, n.Updated = Deleting, "", p.opts.Now()
	if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
		monitor.Problem(p.opts.Logger, "deleting %s: %v", n.Name, err)
		return err
	}
	p.background(func() { p.finishRemoving(n) })
	return nil
}

// finishRemoving has the host take the network away (containers, volumes,
// database), then forgets it and its sign-ins. If the host fails, the
// network is kept, marked failed, so deleting can be tried again.
func (p *Platform) finishRemoving(n Network) {
	defer monitor.Recover()
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := p.opts.Host.Remove(ctx, n.Name); err != nil {
		monitor.Problem(p.opts.Logger, "removing %s: %v", n.Name, err)
		n.Status, n.Problem, n.Updated = Failed, deleteProblem, p.opts.Now()
		if err := p.opts.Networks.UpdateNetwork(ctx, n); err != nil {
			monitor.Problem(p.opts.Logger, "removing %s: %v", n.Name, err)
		}
		return
	}
	if err := p.forgetNetwork(ctx, n); err != nil {
		return // it said why; SweepDesks tries again
	}
	p.opts.Logger.Printf("network %s: deleted", n.Name)
}

// removeFromHome is deleting a network from its page on the home page: for
// one whose setup or deletion failed, or whose owner can't get into its
// console.
func (p *Platform) removeFromHome(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ownNetwork(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != n.Name {
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, http.StatusBadRequest, "home-network", withError{Err: "Type the network's name, " + n.Name + ", to delete it.", View: p.view(r, n)})
		return
	}
	if refusal, err := p.mayDelete(r.Context(), n); err == nil && refusal != "" {
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, http.StatusBadRequest, "home-network", withError{Err: refusal, View: p.view(r, n)})
		return
	}
	if err := p.startRemoving(r.Context(), n); err != nil {
		p.letFormsReach(w, r, n)
		p.renderHome(w, r, http.StatusInternalServerError, "home-network", withError{Err: "Couldn't delete it just now. Try again in a moment.", View: p.view(r, n)})
		return
	}
	http.Redirect(w, r, "/networks", http.StatusSeeOther)
}
