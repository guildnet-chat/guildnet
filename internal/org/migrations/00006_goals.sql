-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- What each team is for, in a line.
ALTER TABLE teams ADD COLUMN focus text NOT NULL DEFAULT '';

-- The company's description: what it does, for whom, and how.
CREATE TABLE company (
    id          int PRIMARY KEY CHECK (id = 1),
    description text        NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The company's goals (team '') and teams' objectives.
CREATE TABLE goals (
    id         bigserial PRIMARY KEY,
    team       text        NOT NULL DEFAULT '',
    text       text        NOT NULL,
    measure    text        NOT NULL DEFAULT '',
    target     text        NOT NULL DEFAULT '',
    current    text        NOT NULL DEFAULT '',
    serves     bigint      NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE goals;
DROP TABLE company;
ALTER TABLE teams DROP COLUMN focus;
