-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- The getting-started steps each network's owner ticked off (or "dismissed",
-- for the whole checklist).
create table onboarding (
    network text not null references networks (name) on delete cascade,
    step    text not null,
    done_at timestamptz not null,
    primary key (network, step)
);

-- +goose Down
drop table onboarding;
