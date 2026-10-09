-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Where an outside agent's wake-ups are sent, and the secret they're signed
-- with, sealed by the runner. One per agent; it goes when the agent is fired.
CREATE TABLE agent_webhooks (
    key        text PRIMARY KEY REFERENCES agents (key) ON DELETE CASCADE,
    url        text  NOT NULL,
    secret     bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE agent_webhooks;
