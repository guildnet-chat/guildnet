-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Two-step sign-in to a network's console, by account: the authenticator
-- app's secret (sealed), the last 30-second step whose code was used (so a
-- code works once), and the hashes of the recovery codes not yet used. It
-- goes with its network.
create table two_step (
    network   text        not null references networks (name) on delete cascade,
    account   text        not null,
    secret    bytea,
    last_step bigint      not null default 0,
    recovery  text[]      not null default '{}',
    since     timestamptz not null,
    primary key (network, account)
);

-- +goose Down
drop table two_step;
