-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- People's IRC app passwords: one for each IRC app, made on their account
-- page and shown once, kept hashed as Ergo hashes passwords. An account
-- with two-step sign-in signs in over IRC only with these.
create table irc_app_passwords (
    network text        not null references networks (name) on delete cascade,
    account text        not null,
    id      text        not null, -- the password's first part, to find it by
    name    text        not null,
    hash    bytea       not null,
    created timestamptz not null,
    used    timestamptz,
    primary key (network, account, id)
);

-- +goose Down
drop table irc_app_passwords;
