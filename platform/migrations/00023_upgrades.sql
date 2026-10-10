-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Upgrades the console makes to what's kept, once each, in Go (they need
-- the platform's key): "seal v1" seals every secret again bound to where
-- it's kept, and keys the hashes tokens are kept by.
create table upgrades (
    name    text        primary key,
    done_at timestamptz not null default now()
);

-- +goose Down
drop table upgrades;
