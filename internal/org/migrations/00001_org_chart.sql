-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Agents and members share one set of names, compared lowercase.
CREATE TABLE org_names (
    key  text PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('agent', 'member'))
);

CREATE TABLE agents (
    key              text PRIMARY KEY REFERENCES org_names (key) ON DELETE CASCADE,
    nick             text          NOT NULL,
    name             text          NOT NULL DEFAULT '',
    role             text          NOT NULL DEFAULT '',
    lane             text          NOT NULL DEFAULT '',
    reports_to       text          NOT NULL DEFAULT '',
    model            text          NOT NULL,
    effort           text          NOT NULL DEFAULT '',
    daily_budget_usd numeric(12,2) NOT NULL DEFAULT 0,
    channels         text[]        NOT NULL DEFAULT '{}',
    prompt           text          NOT NULL DEFAULT '',
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now()
);

CREATE TABLE members (
    key              text PRIMARY KEY REFERENCES org_names (key) ON DELETE CASCADE,
    account          text          NOT NULL,
    name             text          NOT NULL DEFAULT '',
    role             text          NOT NULL DEFAULT '',
    lane             text          NOT NULL DEFAULT '',
    reports_to       text          NOT NULL DEFAULT '',
    guest            boolean       NOT NULL DEFAULT false,
    monthly_cost_usd numeric(12,2) NOT NULL DEFAULT 0,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now()
);

CREATE TABLE lanes (
    key                text PRIMARY KEY,
    name               text          NOT NULL,
    channel            text          NOT NULL DEFAULT '',
    lead               text          NOT NULL DEFAULT '',
    private            boolean       NOT NULL DEFAULT false,
    monthly_budget_usd numeric(12,2) NOT NULL DEFAULT 0,
    created_at         timestamptz   NOT NULL DEFAULT now(),
    updated_at         timestamptz   NOT NULL DEFAULT now()
);

-- One row once guildnet.yaml's starting org chart has been imported; after
-- that the database is the source of truth.
CREATE TABLE org_seeded (
    id        int PRIMARY KEY CHECK (id = 1),
    seeded_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE org_seeded;
DROP TABLE lanes;
DROP TABLE members;
DROP TABLE agents;
DROP TABLE org_names;
