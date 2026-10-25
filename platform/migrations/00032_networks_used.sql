-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- When a person last used each network (its console, web chat, an IRC
-- app), for putting networks nobody uses to sleep. Networks there already
-- count from now.
alter table networks add column used_at timestamptz not null default now();

-- +goose Down
alter table networks drop column used_at;
