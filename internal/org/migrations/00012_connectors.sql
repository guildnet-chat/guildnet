-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- The connectors a team uses (from those the network's owner connected).
ALTER TABLE teams ADD COLUMN connectors text[] NOT NULL DEFAULT '{}';

-- What the network says about each connector tool: its policy (allow, ask,
-- deny; '' for the connector's default), and the hash of the definition
-- people approved, beside the one last seen.
CREATE TABLE connector_tools (
    connector  text NOT NULL,
    tool       text NOT NULL,
    policy     text NOT NULL DEFAULT '',
    approved   text NOT NULL DEFAULT '',
    seen       text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connector, tool)
);

-- +goose Down
DROP TABLE connector_tools;
ALTER TABLE teams DROP COLUMN connectors;
