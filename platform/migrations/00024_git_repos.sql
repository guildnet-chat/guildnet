-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Git repositories on any host that a network's agents work on at desks:
-- where each is, and two credentials, one to read (it reaches desks) and
-- one to push (only the runner has it, for a push). The tokens are sealed,
-- bound to the repository's address, so one can't be pointed elsewhere.
create table git_repos (
    network     text        not null references networks (name) on delete cascade,
    name        text        not null,
    url         text        not null,
    read_user   text        not null default '',
    read_token  bytea,
    write_user  text        not null default '',
    write_token bytea,
    added_by    text        not null,
    added_at    timestamptz not null,
    primary key (network, name)
);

-- +goose Down
drop table git_repos;
