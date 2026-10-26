-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up
-- The call's whole input, as JSON: what the approver decides on, however
-- long (its summary has only the start).
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS input text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE approvals DROP COLUMN input;
