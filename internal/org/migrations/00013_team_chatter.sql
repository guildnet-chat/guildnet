-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- How much of its agents' work a team's channel sees: quiet, summary or
-- steps; '' for that of the team it's inside.
ALTER TABLE teams ADD COLUMN chatter text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE teams DROP COLUMN chatter;
