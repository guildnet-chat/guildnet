-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- A team can have several leads, and sit inside another team.
ALTER TABLE teams ADD COLUMN leads text[] NOT NULL DEFAULT '{}';
UPDATE teams SET leads = ARRAY[lead] WHERE lead <> '';
ALTER TABLE teams DROP COLUMN lead;
ALTER TABLE teams ADD COLUMN parent text NOT NULL DEFAULT '';

-- The access levels a person has been given (Admin, or one the owner made).
ALTER TABLE members ADD COLUMN access text[] NOT NULL DEFAULT '{}';

-- Access levels the owner changed or made; the presets live in the code.
CREATE TABLE access_levels (
    key        text PRIMARY KEY,
    name       text        NOT NULL,
    abilities  text[]      NOT NULL DEFAULT '{}',
    scope      text        NOT NULL,
    teams      text[]      NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every change to the org, only ever added to.
CREATE TABLE org_log (
    id     bigserial PRIMARY KEY,
    at     timestamptz NOT NULL,
    actor  text        NOT NULL,
    action text        NOT NULL,
    target text        NOT NULL DEFAULT '',
    team   text        NOT NULL DEFAULT '',
    detail text        NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE org_log;
DROP TABLE access_levels;
ALTER TABLE members DROP COLUMN access;
ALTER TABLE teams DROP COLUMN parent;
ALTER TABLE teams ADD COLUMN lead text NOT NULL DEFAULT '';
UPDATE teams SET lead = leads[1] WHERE cardinality(leads) > 0;
ALTER TABLE teams DROP COLUMN leads;
