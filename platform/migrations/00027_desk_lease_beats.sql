-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- When each desk's runner last said it was working: a lease not heard from
-- in a few minutes is lost, and charged up to then.
alter table desk_leases add column beat timestamptz;
update desk_leases set beat = started;
alter table desk_leases alter column beat set not null;
drop index desk_leases_network;
create index desk_leases_network on desk_leases (network, beat);
create index desk_leases_beat on desk_leases (beat);

-- +goose Down
drop index desk_leases_beat;
drop index desk_leases_network;
create index desk_leases_network on desk_leases (network, started);
alter table desk_leases drop column beat;
