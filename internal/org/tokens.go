// SPDX-License-Identifier: AGPL-3.0-only

package org

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// AgentTokens are outside agents' tokens, kept only as hashes: one per
// agent, replaced by a new one, and gone when the agent is fired. Every
// Store is one.
type AgentTokens interface {
	// SetAgentToken makes hash the agent's token, in place of any other.
	SetAgentToken(ctx context.Context, nick, hash string) error
	// AgentForToken is whose token hash is, or ErrNotFound.
	AgentForToken(ctx context.Context, hash string) (string, error)
}

// AgentWebhooks are where outside agents' wake-ups are sent, with the
// secret (sealed by the runner) they're signed with. Every Store is one.
type AgentWebhooks interface {
	// SetAgentWebhook sends the agent's wake-ups to url, signed with secret;
	// url "" goes back to the inbox.
	SetAgentWebhook(ctx context.Context, nick, url string, secret []byte) error
	// AgentWebhook is the agent's webhook, or "" for none.
	AgentWebhook(ctx context.Context, nick string) (url string, secret []byte, err error)
}

type webhook struct {
	url    string
	secret []byte
}

func (m *Memory) SetAgentWebhook(_ context.Context, nick, url string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[key(nick)]; !ok {
		return ErrNotFound
	}
	if m.webhooks == nil {
		m.webhooks = map[string]webhook{}
	}
	if url == "" {
		delete(m.webhooks, key(nick))
		return nil
	}
	m.webhooks[key(nick)] = webhook{url, append([]byte(nil), secret...)}
	return nil
}

func (m *Memory) AgentWebhook(_ context.Context, nick string) (string, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.webhooks[key(nick)]
	if !ok {
		return "", nil, nil
	}
	return w.url, append([]byte(nil), w.secret...), nil
}

func (p *Postgres) SetAgentWebhook(ctx context.Context, nick, url string, secret []byte) error {
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE key = $1)`, key(nick)).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if url == "" {
		_, err := p.pool.Exec(ctx, `DELETE FROM agent_webhooks WHERE key = $1`, key(nick))
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO agent_webhooks (key, url, secret) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET url = $2, secret = $3, created_at = now()`, key(nick), url, secret)
	return err
}

func (p *Postgres) AgentWebhook(ctx context.Context, nick string) (string, []byte, error) {
	var url string
	var secret []byte
	err := p.pool.QueryRow(ctx, `SELECT url, secret FROM agent_webhooks WHERE key = $1`, key(nick)).Scan(&url, &secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return url, secret, err
}

func (m *Memory) SetAgentToken(_ context.Context, nick, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[key(nick)]; !ok {
		return ErrNotFound
	}
	if m.tokens == nil {
		m.tokens = map[string]string{}
	}
	m.tokens[key(nick)] = hash
	return nil
}

func (m *Memory) AgentForToken(_ context.Context, hash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, h := range m.tokens {
		if h == hash {
			if a, ok := m.agents[k]; ok {
				return a.Nick, nil
			}
		}
	}
	return "", ErrNotFound
}

func (p *Postgres) SetAgentToken(ctx context.Context, nick, hash string) error {
	tag, err := p.pool.Exec(ctx, `INSERT INTO agent_tokens (key, hash) SELECT key, $2 FROM agents WHERE key = $1
		ON CONFLICT (key) DO UPDATE SET hash = $2, created_at = now()`, key(nick), hash)
	return notFoundIfNone(tag.RowsAffected(), err)
}

func (p *Postgres) AgentForToken(ctx context.Context, hash string) (string, error) {
	var nick string
	err := p.pool.QueryRow(ctx, `SELECT a.nick FROM agent_tokens t JOIN agents a ON a.key = t.key WHERE t.hash = $1`, hash).Scan(&nick)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return nick, err
}
