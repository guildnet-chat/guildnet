-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up
-- When the agent carried on after a decision: a decision made just before a
-- restart is still carried on with after it, and only once. Decisions made
-- before this count as carried on with.
ALTER TABLE approvals ADD COLUMN resumed_at timestamptz;
UPDATE approvals SET resumed_at = coalesce(decided_at, now()) WHERE status <> 'pending';
CREATE INDEX approvals_unresumed ON approvals (id) WHERE status <> 'pending' AND resumed_at IS NULL;

-- +goose Down
DROP INDEX approvals_unresumed;
ALTER TABLE approvals DROP COLUMN resumed_at;
