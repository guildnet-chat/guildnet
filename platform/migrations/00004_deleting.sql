-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up
-- A network being deleted: its containers and database are being removed.
alter table networks drop constraint networks_status_check;
alter table networks add constraint networks_status_check check (status in ('creating', 'ready', 'failed', 'deleting'));

-- +goose Down
update networks set status = 'failed' where status = 'deleting';
alter table networks drop constraint networks_status_check;
alter table networks add constraint networks_status_check check (status in ('creating', 'ready', 'failed'));
