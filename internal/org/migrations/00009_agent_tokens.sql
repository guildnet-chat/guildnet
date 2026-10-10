-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- An outside agent's token, as a SHA-256 hash (the token itself is shown
-- once and never kept). One per agent; it goes when the agent is fired.
CREATE TABLE agent_tokens (
    key        text PRIMARY KEY REFERENCES agents (key) ON DELETE CASCADE,
    hash       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE agent_tokens;
