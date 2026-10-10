-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Web chat: where each network's Ergo takes WebSocket connections, and the
-- key (sealed) that signs the tokens it accepts in place of a password.
alter table networks add column chat_url text not null default '';
alter table networks add column chat_key bytea;

-- +goose Down
alter table networks drop column chat_key;
alter table networks drop column chat_url;
