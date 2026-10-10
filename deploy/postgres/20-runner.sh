#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# A self-hosted install's runner: a database and login of its own, like the
# platform's (10-platform.sh, which says how it runs), when
# RUNNER_DB_PASSWORD is set. (On guildnet.chat each network's runner gets
# its own from the host.)
set -eu
[ -n "${RUNNER_DB_PASSWORD:-}" ] || exit 0
psql -v ON_ERROR_STOP=1 -v pw="$RUNNER_DB_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
select 'create role runner login' where not exists (select from pg_roles where rolname = 'runner') \gexec
alter role runner login password :'pw';
grant runner to current_user;
select 'create database runner owner runner' where not exists (select from pg_database where datname = 'runner') \gexec
-- Closed to every other login, by its owner (only an owner's revoke counts).
set role runner;
revoke connect on database runner from public;
reset role;
revoke runner from current_user;
SQL