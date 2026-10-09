-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- What GuildNet's admins keep about people: when they were last signed in
-- on the home page, and whether they're blocked from signing in.
alter table people
    add column last_seen timestamptz,
    add column blocked   boolean not null default false;

-- A network suspended by the admins: stopped, with everything kept.
alter table networks drop constraint networks_status_check;
alter table networks add constraint networks_status_check check (status in ('creating', 'ready', 'failed', 'deleting', 'suspended'));

-- What the admins did, and which of them did it.
create table admin_log (
    id     bigserial primary key,
    at     timestamptz not null,
    admin  text not null,
    action text not null,
    target text not null default ''
);

-- +goose Down
drop table admin_log;
update networks set status = 'ready' where status = 'suspended';
alter table networks drop constraint networks_status_check;
alter table networks add constraint networks_status_check check (status in ('creating', 'ready', 'failed', 'deleting'));
alter table people drop column last_seen, drop column blocked;
