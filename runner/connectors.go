// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Connectors (Linear, Notion...) are connected by the network's owner on
// the platform. Here: which a team uses (set like its repos, by whoever may
// arrange the team), and what each tool may do without asking and whether
// its definition is approved (set by whoever may approve anything in the
// org, since it decides what needs approval).

// Connectors are the network's connected services, as the console sees and
// sets them.
type Connectors interface {
	Services() []connectors.Service
	SetPolicy(ctx context.Context, connector, tool, policy string) error
	Approve(ctx context.Context, connector, tool string) error
}

// UseConnectors gives the runner the network's connectors; nil for none.
func (r *Runner) UseConnectors(c Connectors) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectors = c
}

func (r *Runner) services() []connectors.Service {
	r.mu.Lock()
	c := r.connectors
	r.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Services()
}

// ConnectorsFor is the connectors a team uses: its own, or those of the
// nearest team it's inside that has some.
func (r *Runner) ConnectorsFor(team string) []string {
	return org.ConnectorsFor(context.Background(), r.store, team)
}

// teamConnectors reads a team's CONNECTORS field: connected services, by ID
// or title.
func (r *Runner) teamConnectors(value string) ([]string, string) {
	if value == "" || strings.EqualFold(value, "none") {
		return nil, ""
	}
	have := r.services()
	if len(have) == 0 {
		return nil, "No services are connected to this network yet: the owner connects them on the network's page at GuildNet."
	}
	var ids, titles []string
	for _, s := range have {
		titles = append(titles, s.Title)
	}
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		i := slices.IndexFunc(have, func(s connectors.Service) bool {
			return strings.EqualFold(s.ID, name) || strings.EqualFold(s.Title, name)
		})
		if i < 0 {
			return nil, fmt.Sprintf("%s isn't connected to this network: %s.", name, strings.Join(titles, ", "))
		}
		if !slices.Contains(ids, have[i].ID) {
			ids = append(ids, have[i].ID)
		}
	}
	return ids, ""
}

func (r *Runner) apiConnectors(w http.ResponseWriter, _ *http.Request) {
	list := r.services()
	if list == nil {
		list = []connectors.Service{}
	}
	apiJSON(w, http.StatusOK, map[string]any{"connectors": list})
}

// apiSetConnectorTool sets a tool's policy, or approves its definition.
func (r *Runner) apiSetConnectorTool(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Policy  *string `json:"policy"`
		Approve bool    `json:"approve"`
	}
	if !decode(w, req, &in) {
		return
	}
	actor := actorOf(req)
	connector, tool := strings.ToLower(req.PathValue("connector")), req.PathValue("tool")
	if err := r.allowed(actor, canApprove, wholeOrg, "set what connectors' tools may do"); err != nil {
		apiRefusal(w, err)
		return
	}
	r.mu.Lock()
	c := r.connectors
	r.mu.Unlock()
	if c == nil {
		apiError(w, http.StatusNotFound, "no services are connected to this network")
		return
	}
	var err error
	var detail []string
	if in.Policy != nil {
		if err = c.SetPolicy(req.Context(), connector, tool, *in.Policy); err == nil {
			p := *in.Policy
			if p == "" {
				p = "the default"
			}
			detail = append(detail, "policy "+p)
		}
	}
	if err == nil && in.Approve {
		if err = c.Approve(req.Context(), connector, tool); err == nil {
			detail = append(detail, "approved as it is now")
		}
	}
	switch {
	case errors.Is(err, connectors.ErrNoTool):
		apiError(w, http.StatusNotFound, fmt.Sprintf("%s has no tool %s", connector, tool))
		return
	case errors.Is(err, connectors.ErrBadPolicy):
		apiError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		monitor.Problem(r.logger, "setting %s's %s: %v", connector, tool, err)
		apiError(w, http.StatusInternalServerError, "couldn't save that; try again")
		return
	}
	if len(detail) > 0 {
		r.logChange(actor, "set connector tool", connector+" "+tool, "", strings.Join(detail, "; "))
	}
	apiJSON(w, http.StatusOK, map[string]any{"connectors": r.services()})
}
