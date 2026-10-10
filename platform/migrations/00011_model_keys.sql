-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- A network's keys for each model provider (Anthropic, OpenAI, Google,
-- OpenRouter), sealed together. anthropic_key stays filled too, so a
-- network saved before this still has its key, and older code still reads.
alter table networks add column model_keys bytea;

-- +goose Down
alter table networks drop column model_keys;
