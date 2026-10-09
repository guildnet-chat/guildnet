-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- GuildNet as a client of services' authorization servers: registered once
-- per server and callback address, for every network. The secret is sealed.
create table oauth_clients (
    issuer        text  not null,
    redirect_uri  text  not null,
    client_id     text  not null,
    client_secret bytea,
    auth_method   text  not null,
    primary key (issuer, redirect_uri)
);

-- Networks' connections to connectors (Linear, Notion...): where the
-- service's authorization server is, GuildNet's client there, and the
-- tokens, sealed. reconnect: the service refused the refresh token.
create table connections (
    network       text        not null references networks (name) on delete cascade,
    connector     text        not null,
    server        jsonb       not null,
    client_id     text        not null,
    client_secret bytea,
    auth_method   text        not null,
    access_token  bytea       not null,
    refresh_token bytea,
    expires_at    timestamptz,
    scope         text        not null default '',
    reconnect     boolean     not null default false,
    connected_by  text        not null,
    connected_at  timestamptz not null,
    primary key (network, connector)
);

-- +goose Down
drop table connections;
drop table oauth_clients;
