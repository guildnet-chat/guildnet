-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- A network's GitHub installation (of GuildNet's public app). One network
-- per installation: the unique index makes sure it's never bound to two.
create table network_github (
    network      text primary key references networks (name) on delete cascade,
    installation bigint      not null unique,
    account      text        not null default '',
    connected_by text        not null default '',
    connected_at timestamptz not null
);

-- +goose Down
drop table network_github;