-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Lanes are called teams now.
ALTER TABLE links RENAME COLUMN lane TO team;

-- +goose Down
ALTER TABLE links RENAME COLUMN team TO lane;
