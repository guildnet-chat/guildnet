#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# The platform's own database and login, kept apart from the runner's: each
# login can connect only to its own database. Run by the image on a new
# volume, or against managed Postgres (Cloud SQL, RDS) as its admin, who
# isn't a superuser (PGHOST and PGPASSWORD set; POSTGRES_DB=postgres). Safe
# to run again: it changes only the login's password.
set -eu
psql -v ON_ERROR_STOP=1 -v pw="$PLATFORM_DB_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
select 'create role platform login' where not exists (select from pg_roles where rolname = 'platform') \gexec
alter role platform login password :'pw';
-- Making a database another login owns takes acting as it, for a moment:
-- an admin that isn't a superuser can't otherwise.
grant platform to current_user;
select 'create database platform owner platform' where not exists (select from pg_database where datname = 'platform') \gexec
-- Closed to every other login, by its owner (only an owner's revoke counts).
set role platform;
revoke connect on database platform from public;
reset role;
revoke platform from current_user;
-- The admin's own database, closed to the others where it's the admin's.
select format('revoke connect on database %I from public', current_database())
  where (select datdba from pg_database where datname = current_database()) = (select oid from pg_roles where rolname = current_user) \gexec
SQL