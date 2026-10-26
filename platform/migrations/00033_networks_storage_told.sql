-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up
-- How full a network's desks' storage was, in percent (80 or 100), when its
-- owner was last told; 0 once it dropped back.
alter table networks add column if not exists storage_told integer not null default 0;

-- +goose Down
alter table networks drop column storage_told;
