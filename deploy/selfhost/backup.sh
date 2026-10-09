#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Backs up everything this install keeps into backups/<time>/ beside this
# file:
#
#   env              .env: its secrets, without which the rest can't be read
#   brand.tar        your own files (brand/)
#   platform.dump    the console's database: accounts, the network's
#                    settings, its model keys (sealed), invites, connectors
#   runner.dump      the runner's: the org chart, agents, approvals, the log
#   files.tar        Ergo's files (IRC accounts, channels and chat history)
#                    and the runner's (the spend ledger)
#
# Ergo and the runner are paused while their files are copied (a second or
# two; nobody is disconnected), so each copy is whole. The backup holds the
# install's secrets: keep it private, and copy it off this machine.
# restore.sh puts one back.
set -eu
cd "$(dirname "$0")"
# On Windows, Git Bash (and MSYS2) would rewrite /files, a path inside a
# container, into a Windows one before docker saw it.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'
umask 077
dir=backups/$(date -u +%Y-%m-%dT%H%M%SZ)
mkdir -p "$dir"

cp .env "$dir/env"
tar -cf "$dir/brand.tar" brand
for db in platform runner; do
  docker compose exec -T postgres pg_dump -U guildnet --format=custom "$db" > "$dir/$db.dump"
done

# The files service runs Postgres's image, which is here: nothing is fetched
# or built, whose progress would go into files.tar.
docker compose pause ergo runner >/dev/null
trap 'docker compose unpause ergo runner >/dev/null' EXIT
docker compose run --rm -T files -cf - -C /files . > "$dir/files.tar"
docker compose unpause ergo runner >/dev/null
trap - EXIT
# What was copied is whole, or it isn't a backup.
if ! tar -tf "$dir/files.tar" >/dev/null 2>&1; then
  echo "backup.sh: the copy of Ergo's and the runner's files ($dir/files.tar) isn't whole, so this isn't a backup: try again" >&2
  exit 1
fi

echo "backed up to $dir ($(du -sh "$dir" | cut -f1)): keep it private, and copy it off this machine"
