-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Passkeys added as a second step to a network's console: the credential
-- (its public key and sign count, as JSON; nothing secret) by its ID. They
-- go with their network.
create table passkeys (
    network    text        not null references networks (name) on delete cascade,
    account    text        not null,
    id         text        not null,
    name       text        not null,
    credential bytea       not null,
    added      timestamptz not null,
    used       timestamptz,
    primary key (network, account, id)
);

-- +goose Down
drop table passkeys;
