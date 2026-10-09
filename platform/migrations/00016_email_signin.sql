-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- People may sign in by email, without a Google account.
alter table people alter column google_sub drop not null;
create index people_email on people (email);

-- +goose Down
drop index people_email;
delete from people where google_sub is null;
alter table people alter column google_sub set not null;
