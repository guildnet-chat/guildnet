// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Connectors of a network's own: any MCP server its owner adds, from the
// console. One with the MCP sign-in is connected that way; any other with a
// token the owner pastes (an API key), tried on the server first and kept
// sealed like the rest. Not the catalogue's, none of its tools is known to
// only read, so each asks a person first (the runner's side) until someone
// who approves says otherwise. It's kept with its connection: disconnecting
// it forgets it.

// maxCustom is how many a network may add.
const maxCustom = 10

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// customID is what a connector named title goes by: in its tools' names.
func customID(title string) string {
	id := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(id) > 30 {
		id = strings.TrimRight(id[:30], "-")
	}
	return id
}

// connectorOf is the network's connector id: the catalogue's, or one its
// owner added.
func (p *Platform) connectorOf(ctx context.Context, network, id string) (connectors.Connector, bool) {
	if c, ok := connectors.Find(p.catalogue(), id); ok {
		return c, true
	}
	if p.opts.Connections == nil {
		return connectors.Connector{}, false
	}
	c, err := p.opts.Connections.Connection(ctx, network, id)
	if err != nil || c.Custom == nil {
		return connectors.Connector{}, false
	}
	return *c.Custom, true
}

// checkCustom is a connector of the network's own named title at raw, or
// what's wrong with it.
func (p *Platform) checkCustom(ctx context.Context, network, title, raw string) (connectors.Connector, string) {
	title = strings.Join(strings.Fields(title), " ")
	id := customID(title)
	switch {
	case id == "" || len(title) > 40:
		return connectors.Connector{}, "Name it in up to 40 characters, with some letters or digits."
	case connectorTaken(p.catalogue(), id):
		return connectors.Connector{}, "There's already a connector called " + title + ": name yours something else."
	}
	if p.opts.Connections != nil {
		have, err := p.opts.Connections.Connections(ctx, network)
		if err != nil {
			return connectors.Connector{}, "Couldn't read the network's connectors. Try again."
		}
		custom := 0
		for _, h := range have {
			if strings.EqualFold(h.Connector, id) {
				return connectors.Connector{}, "There's already a connector called " + title + ": name yours something else."
			}
			if h.Custom != nil {
				custom++
			}
		}
		if custom >= maxCustom {
			return connectors.Connector{}, "A network may add 10 servers of its own: disconnect one first."
		}
	}
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	switch {
	case err == nil && u.Scheme == "http":
		return connectors.Connector{}, "Use the server's https:// address: over http:// its token would be sent unencrypted."
	case err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "":
		return connectors.Connector{}, "That isn't an MCP server's https:// address."
	}
	if err := p.opts.ConnectorEgress.Check(raw); err != nil {
		problem := u.Hostname() + " is a private address (or a name inside a network), which " + p.brand().Name + " doesn't reach."
		if p.opts.FirstRun != nil {
			problem += " Whoever runs it can allow one on your own network with GUILDNET_CONNECTOR_PRIVATE_HOSTS."
		}
		return connectors.Connector{}, problem
	}
	return connectors.Connector{ID: id, Title: title, About: "Added by the owner: " + u.Hostname(), URL: raw, Custom: true}, ""
}

// connectorTaken says whether the catalogue has id.
func connectorTaken(list []connectors.Connector, id string) bool {
	_, ok := connectors.Find(list, id)
	return ok
}

// addCustomConnector adds an MCP server of the network's own: with the token
// given, tried first; without one, through its sign-in.
func (p *Platform) addCustomConnector(w http.ResponseWriter, r *http.Request) {
	if p.opts.Connections == nil {
		http.NotFound(w, r)
		return
	}
	n := networkOf(r)
	c, problem := p.checkCustom(r.Context(), n.Name, r.PostFormValue("title"), r.PostFormValue("url"))
	if problem != "" {
		p.showConnectors(w, r, http.StatusBadRequest, problem)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		authorize, err := p.startConnect(w, r, n, c, byAccount(r), true)
		if err != nil {
			p.showConnectors(w, r, http.StatusBadRequest, c.Title+" doesn't offer the MCP sign-in (or couldn't be reached): give a token for it (an API key) instead.")
			return
		}
		p.render(w, r, http.StatusOK, "connect-onward", onward{Title: c.Title, Network: n.Name, URL: authorize})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tools, err := connectors.ListTools(ctx, p.connectorHTTP, c.URL, token)
	if err != nil {
		p.opts.Logger.Printf("adding %s's %s: %v", n.Name, c.ID, err)
		p.showConnectors(w, r, http.StatusBadRequest, c.Title+" didn't take the token, or isn't an MCP server: check its address and the token.")
		return
	}
	err = p.opts.Connections.SaveConnection(r.Context(), n.Name, Connection{Connector: c.ID, Custom: &c,
		Tokens: connectors.Tokens{Access: token}, By: byAccount(r), At: p.opts.Now()})
	if err != nil {
		monitor.Problem(p.opts.Logger, "adding %s's %s: %v", n.Name, c.ID, err)
		p.showConnectors(w, r, http.StatusInternalServerError, "Couldn't save the connection. Try again.")
		return
	}
	p.opts.Logger.Printf("%s added %s (%d tools)", n.Name, c.ID, len(tools))
	p.tellOwner(r.Context(), n, connectorEmail(n, p.networkOrigin(r, n.Name)+"/connectors", "connected", c.Title, inConsole(r), p.opts.Now()))
	http.Redirect(w, r, "/connectors#"+url.PathEscape(c.ID), http.StatusSeeOther)
}
