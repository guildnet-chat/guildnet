-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Where someone in a network wants email alerts, and which. An address is
-- only used once they've confirmed it (email is the confirmed one; pending
-- waits for its link, whose secret is kept only as a hash). Approval emails
-- are off until they turn them on. It goes with its network.
create table alerts (
    network       text        not null references networks (name) on delete cascade,
    account       text        not null,
    email         text        not null default '',
    pending       text        not null default '',
    confirm_hash  text        unique,
    confirm_until timestamptz,
    approvals     boolean     not null default false,
    primary key (network, account)
);

-- +goose Down
drop table alerts;
