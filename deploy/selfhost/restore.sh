#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Puts a backup (from backup.sh) back, into an install with no data yet: a
# new machine, say, with this repo checked out at the same release or a
# later one. Then starts it.
#
#   sh restore.sh backups/<time>
#
# The backup's .env becomes this install's: its secrets are what the data
# was made with.
set -eu
# On Windows, Git Bash (and MSYS2) would rewrite /files, a path inside a
# container, into a Windows one before docker saw it.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'
[ $# -eq 1 ] || { echo "usage: sh restore.sh backups/<time>" >&2; exit 2; }
# The backup, wherever it is (from here, or a full path).
from=$(cd "$1" 2>/dev/null && pwd) || { echo "restore.sh: there's no $1" >&2; exit 1; }
cd "$(dirname "$0")"
for f in env brand.tar platform.dump runner.dump files.tar; do
  [ -f "$from/$f" ] || { echo "restore.sh: $from has no $f: is it a backup from backup.sh?" >&2; exit 1; }
done
# Nothing in a backup is trusted but what a backup has. Its env holds only
# the stack's settings (Compose reads others, like COMPOSE_FILE, from .env);
# brand.tar only files and folders under brand/ (not compose.yml, say).
untrusted() { echo "restore.sh: $from's $1, so it wasn't restored: is it as backup.sh made it?" >&2; exit 1; }
if grep -vE '^(#.*|[[:space:]]*|GUILDNET_[A-Z0-9_]+=.*|COMPOSE_PROFILES=desks)$' "$from/env" >/dev/null; then
  untrusted "env has lines that aren't GuildNet's settings"
fi
if tar -tf "$from/brand.tar" | grep -vE '^(\./)?brand(/[A-Za-z0-9._-]+)*/?$' >/dev/null ||
   tar -tf "$from/brand.tar" | grep -E '(^|/)\.\.(/|$)' >/dev/null; then
  untrusted "brand.tar has more than brand/"
fi
if tar -tvf "$from/brand.tar" | cut -c1 | grep -v '[-d]' >/dev/null; then
  untrusted "brand.tar has links"
fi
# Where the stack's images come from isn't a backup's to say: one changed
# where it was kept could otherwise have this machine run its own.
if grep -Eq '^GUILDNET_(REGISTRY|[A-Z_]*IMAGE)=' "$from/env"; then
  untrusted "env says where images come from (GUILDNET_REGISTRY, or an image): if you set that yourself, take the line out of $from/env, restore, and put it back in .env after"
fi

umask 077
# This install's .env: the backup's, with the release this checkout's, not
# the backup's (a backup can't take the install back to an older one).
version=$(cat ../../VERSION 2>/dev/null) || version=
env=$(mktemp .env.XXXXXX)
trap 'rm -f "$env"' EXIT
grep -v '^GUILDNET_VERSION=' "$from/env" > "$env" || true
if [ -n "$version" ]; then
  printf '\n# The release this install runs (docs/self-host/operations.md#upgrading).\nGUILDNET_VERSION=%s\n' "$version" >> "$env"
fi
if [ -e .env ] && ! cmp -s .env "$env"; then
  echo "restore.sh: this install has an .env of its own: restore into one without (move it away first)" >&2
  exit 1
fi
mv -f "$env" .env
trap - EXIT
if [ -n "$(docker compose ps -aq)" ]; then
  echo "restore.sh: this install is already running: restore into one with no data (docker compose down -v removes it, data and all)" >&2
  exit 1
fi
tar -xf "$from/brand.tar"
chmod 755 brand

# The databases: Postgres makes them (and their logins) on its new volume,
# then each is filled from its dump.
docker compose up -d --wait postgres
for db in platform runner; do
  docker compose exec -T postgres pg_restore -U guildnet --dbname="$db" --no-owner --role="$db" --exit-on-error < "$from/$db.dump"
done

# Ergo's and the runner's files, into their new volumes, as they were.
docker compose run --rm -T files -xf - -C /files < "$from/files.tar"

docker compose up -d
echo "restored from $from"
