-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- When a sign-in ends if it isn't used before: each use puts it off, never
-- past expires. Null for sign-ins from before, which keep their week.
alter table sessions add column idle_until timestamptz;
create index sessions_idle_until on sessions (idle_until);

-- +goose Down
drop index sessions_idle_until;
alter table sessions drop column idle_until;
