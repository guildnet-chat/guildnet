-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- People who sign in on the home page, with Google. The ID is ours and
-- random; the Google subject is how Google names the account.
create table people (
    id         text primary key,
    google_sub text not null unique,
    email      text not null check (email = lower(email)),
    created_at timestamptz not null default now()
);

-- Networks created on the home page: their setup, owner and settings. The
-- owner's Anthropic key is sealed, like the network's other secrets.
alter table networks
    add column status        text not null default 'ready' check (status in ('creating', 'ready', 'failed')),
    add column problem       text not null default '',
    add column owner         text references people (id),
    add column owner_nick    text not null default '',
    add column template      text not null default '',
    add column timezone      text not null default '',
    add column anthropic_key bytea,
    add column updated_at    timestamptz not null default now();
create index networks_owner on networks (owner);

-- A session on the home page belongs to a person, not a network.
alter table sessions alter column network drop not null;

-- +goose Down
delete from sessions where network is null;
alter table sessions alter column network set not null;
drop index networks_owner;
alter table networks
    drop column status, drop column problem, drop column owner, drop column owner_nick,
    drop column template, drop column timezone, drop column anthropic_key, drop column updated_at;
drop table people;
