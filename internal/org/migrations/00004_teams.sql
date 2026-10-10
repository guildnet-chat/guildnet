-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Lanes are called teams now.
ALTER TABLE lanes RENAME TO teams;
ALTER TABLE agents RENAME COLUMN lane TO team;
ALTER TABLE members RENAME COLUMN lane TO team;

-- +goose Down
ALTER TABLE members RENAME COLUMN team TO lane;
ALTER TABLE agents RENAME COLUMN team TO lane;
ALTER TABLE teams RENAME TO lanes;
