-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up
-- A team's numbers read its approvals of the last 30 days.
CREATE INDEX approvals_raised ON approvals (raised_at);

-- +goose Down
DROP INDEX approvals_raised;
