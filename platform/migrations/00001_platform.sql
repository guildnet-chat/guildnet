-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Networks: one per company. Their secrets are sealed by the platform
-- before they get here; this table never holds them in plain text.
create table networks (
    name         text primary key check (name = lower(name) and name ~ '^[a-z][a-z0-9-]{0,62}$'),
    region       text not null,
    runner_url   text not null,
    runner_token bytea not null,
    ergo_url     text not null,
    ergo_token   bytea not null,
    created_at   timestamptz not null default now()
);

-- Sessions, by the hash of their token (never the token itself). A session
-- belongs to one network and goes with it.
create table sessions (
    hash       text primary key,
    network    text not null references networks (name) on delete cascade,
    account    text not null,
    csrf       text not null,
    expires    timestamptz not null,
    created_at timestamptz not null default now()
);
create index sessions_expires on sessions (expires);

-- +goose Down
drop table sessions;
drop table networks;
