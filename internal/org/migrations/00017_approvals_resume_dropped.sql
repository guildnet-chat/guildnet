-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up
-- What an agent carries on from (its paused conversation) isn't kept once
-- it has carried on.
UPDATE approvals SET resume = '' WHERE resumed_at IS NOT NULL;

-- +goose Down
SELECT 1;
