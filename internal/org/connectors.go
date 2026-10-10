// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"sort"
)

// ConnectorTool is what the network says about one of a connector's tools:
// who may use it without asking (its policy), and the definition people
// approved, pinned by its hash, so a service that changes what a tool says
// or does can't slip the change past them.
type ConnectorTool struct {
	Connector, Tool string
	// Policy is "allow", "ask" or "deny"; "" means the connector's default
	// (reads allow, the rest ask).
	Policy string
	// Approved is the hash of the definition people approved ("" for
	// none yet); Seen is the hash of the definition as last seen.
	Approved, Seen string
}

// ConnectorTools keeps what the network says about connectors' tools.
// Every Store is one.
type ConnectorTools interface {
	// ConnectorTools are a connector's tools as the network has them, by
	// name; connector "" means every connector's.
	ConnectorTools(ctx context.Context, connector string) ([]ConnectorTool, error)
	// SaveConnectorTools adds tools, or changes them in place.
	SaveConnectorTools(ctx context.Context, tools []ConnectorTool) error
}

// ConnectorsFor is the connectors a team uses: its own, or those of the
// nearest team it's inside that has some.
func ConnectorsFor(ctx context.Context, s Store, team string) []string {
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[key(cur)]; {
		seen[key(cur)] = true
		t, err := s.Team(ctx, cur)
		if err != nil {
			return nil
		}
		if len(t.Connectors) > 0 {
			return t.Connectors
		}
		cur = t.Parent
	}
	return nil
}

func toolKey(connector, tool string) string { return key(connector) + "\x00" + tool }

func (m *Memory) ConnectorTools(_ context.Context, connector string) ([]ConnectorTool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ConnectorTool
	for _, t := range m.connectorTools {
		if connector == "" || key(t.Connector) == key(connector) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return toolKey(out[i].Connector, out[i].Tool) < toolKey(out[j].Connector, out[j].Tool)
	})
	return out, nil
}

func (m *Memory) SaveConnectorTools(_ context.Context, tools []ConnectorTool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connectorTools == nil {
		m.connectorTools = map[string]ConnectorTool{}
	}
	for _, t := range tools {
		m.connectorTools[toolKey(t.Connector, t.Tool)] = t
	}
	return nil
}

func (p *Postgres) ConnectorTools(ctx context.Context, connector string) ([]ConnectorTool, error) {
	rows, err := p.pool.Query(ctx, `SELECT connector, tool, policy, approved, seen FROM connector_tools
		WHERE $1 = '' OR connector = $1 ORDER BY connector, tool`, key(connector))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectorTool
	for rows.Next() {
		var t ConnectorTool
		if err := rows.Scan(&t.Connector, &t.Tool, &t.Policy, &t.Approved, &t.Seen); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) SaveConnectorTools(ctx context.Context, tools []ConnectorTool) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, t := range tools {
		if _, err := tx.Exec(ctx, `INSERT INTO connector_tools (connector, tool, policy, approved, seen) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (connector, tool) DO UPDATE SET policy = $3, approved = $4, seen = $5, updated_at = now()`,
			key(t.Connector), t.Tool, t.Policy, t.Approved, t.Seen); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
