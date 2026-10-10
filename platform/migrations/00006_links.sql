-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- One-time links on a network: invitations to join it, and password resets.
-- Only the hash of a link's secret is kept; the id names it to the people
-- who manage it. A link goes with its network.
create table links (
    id         text primary key,
    hash       text not null unique,
    network    text not null references networks (name) on delete cascade,
    kind       text not null check (kind in ('invite', 'reset')),
    account    text not null default '',
    role       text not null default '',
    lane       text not null default '',
    guest      boolean not null default false,
    created_by text not null,
    created_at timestamptz not null,
    expires_at timestamptz not null,
    used_at    timestamptz
);
create index links_network on links (network, kind);

-- +goose Down
drop table links;
