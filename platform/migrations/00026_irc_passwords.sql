-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- People's IRC passwords: the console checks every sign-in, its own and
-- IRC's (Ergo asks it), so guessing is held back the same everywhere and
-- nobody changes a password over IRC. Hashed as Ergo hashes them.
create table irc_passwords (
    network text  not null references networks (name) on delete cascade,
    account text  not null,
    hash    bytea not null,
    primary key (network, account)
);

-- Wrong passwords in the last hour, and where people have signed in from:
-- guessing from one address, or from many, is held back by these.
create table irc_sign_in_failures (
    network text        not null references networks (name) on delete cascade,
    account text        not null,
    source  text        not null,
    at      timestamptz not null
);
create index irc_sign_in_failures_account on irc_sign_in_failures (network, account, at);

create table irc_sign_in_sources (
    network text        not null references networks (name) on delete cascade,
    account text        not null,
    source  text        not null,
    at      timestamptz not null,
    primary key (network, account, source)
);

-- +goose Down
drop table irc_sign_in_sources;
drop table irc_sign_in_failures;
drop table irc_passwords;
