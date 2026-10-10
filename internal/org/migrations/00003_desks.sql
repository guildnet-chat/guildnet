-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Where each agent runs: '' (chat, no tools) or 'managed' (Managed Agents).
ALTER TABLE agents ADD COLUMN platform text NOT NULL DEFAULT '';

-- Each agent's desk on its platform: the platform's agent config and the
-- long-lived session that is its workspace, plus what it has cost so far.
CREATE TABLE desks (
    key           text PRIMARY KEY,
    nick          text   NOT NULL,
    agent_id      text   NOT NULL,
    agent_version bigint NOT NULL,
    session_id    text   NOT NULL,
    cost_cents    bigint NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE desks;
ALTER TABLE agents DROP COLUMN platform;
