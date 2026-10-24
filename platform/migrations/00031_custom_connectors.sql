-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- A connector the network's owner added (any MCP server): the connector
-- itself, as JSON, kept with its connection. Null for the catalogue's.
alter table connections add column custom jsonb;

-- +goose Down
alter table connections drop column custom;
