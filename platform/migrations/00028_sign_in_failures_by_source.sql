-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- Wrong passwords by where they came from: one address guessing at many
-- accounts is held back too.
create index irc_sign_in_failures_source on irc_sign_in_failures (network, source, at);
create index irc_sign_in_failures_at on irc_sign_in_failures (network, at);

-- +goose Down
drop index irc_sign_in_failures_at;
drop index irc_sign_in_failures_source;
