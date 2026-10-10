-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- An agent waiting on a person before a risky action. Kept here so it
-- survives restarts and weekends.
CREATE TABLE approvals (
    id         bigserial PRIMARY KEY,
    agent      text        NOT NULL,
    approver   text        NOT NULL,
    tool       text        NOT NULL DEFAULT '',
    summary    text        NOT NULL,
    channel    text        NOT NULL DEFAULT '',
    asker      text        NOT NULL DEFAULT '',
    resume     text        NOT NULL DEFAULT '',
    status     text        NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    decided_by text        NOT NULL DEFAULT '',
    reason     text        NOT NULL DEFAULT '',
    raised_at  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    decided_at timestamptz
);

CREATE INDEX approvals_pending ON approvals (id) WHERE status = 'pending';

-- +goose Down
DROP TABLE approvals;
