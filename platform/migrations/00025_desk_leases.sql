-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Desks at work: each a network's runner was given a turn for, until it
-- says how long the desk worked. Kept here so a restart of the console
-- doesn't forget them (and the desk's time with them).
create table desk_leases (
    id      text        primary key,
    network text        not null references networks (name) on delete cascade,
    agent   text        not null,
    started timestamptz not null
);
create index desk_leases_network on desk_leases (network, started);

-- +goose Down
drop table desk_leases;
