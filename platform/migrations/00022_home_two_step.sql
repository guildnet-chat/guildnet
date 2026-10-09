-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Two-step sign-in to the home page, by person: the authenticator app's
-- secret (sealed), the last 30-second step whose code was used, and the
-- hashes of the recovery codes not yet used.
create table home_two_step (
    person    text        primary key references people (id) on delete cascade,
    secret    bytea,
    last_step bigint      not null default 0,
    recovery  text[]      not null default '{}',
    since     timestamptz not null
);

-- +goose Down
drop table home_two_step;
