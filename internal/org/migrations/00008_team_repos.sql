-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- The repos a team works on (from the network's GitHub installation).
ALTER TABLE teams ADD COLUMN repos text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE teams DROP COLUMN repos;
