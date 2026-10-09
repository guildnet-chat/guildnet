-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Who an approval asks, in order, and when it moves on to the next.
ALTER TABLE approvals ADD COLUMN route text[] NOT NULL DEFAULT '{}';
ALTER TABLE approvals ADD COLUMN moves_at timestamptz;

-- The org's own settings, by name.
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE settings;
ALTER TABLE approvals DROP COLUMN moves_at;
ALTER TABLE approvals DROP COLUMN route;
