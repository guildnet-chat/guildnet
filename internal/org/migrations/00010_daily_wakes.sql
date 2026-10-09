-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- How many times a day an outside agent may be woken; 0 means the default.
ALTER TABLE agents ADD COLUMN daily_wakes integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE agents DROP COLUMN daily_wakes;
