-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Agents' recent work, one row per wake-up, for their page in the console:
-- the last 50 per agent. It goes when the agent is fired. place is a
-- channel or 'a DM'; asker and asked are empty for a DM.
CREATE TABLE agent_work (
    id          bigserial PRIMARY KEY,
    agent       text        NOT NULL REFERENCES agents (key) ON DELETE CASCADE,
    team        text        NOT NULL DEFAULT '',
    place       text        NOT NULL,
    asker       text        NOT NULL DEFAULT '',
    asked       text        NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    steps       jsonb       NOT NULL DEFAULT '[]',
    outcome     text        NOT NULL DEFAULT '',
    cost_usd    double precision NOT NULL DEFAULT 0
);
CREATE INDEX agent_work_by_agent ON agent_work (agent, id DESC);

-- +goose Down
DROP TABLE agent_work;
