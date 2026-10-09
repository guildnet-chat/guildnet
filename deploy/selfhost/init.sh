#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Makes this install's settings: .env beside this file, with its domain, its
# network's name, the release it runs (this repo's VERSION), and every
# secret the stack needs, made here and never shown. Readable only by
# whoever runs it. Run once:
#
#   sh init.sh <domain> [network]
#
# e.g. sh init.sh chat.example.com hq: the home page (where first-run setup
# happens) is https://chat.example.com and the network's console is
# https://hq.chat.example.com. The network is "hq" unless you say.
set -eu
cd "$(dirname "$0")"

usage() {
  echo "usage: sh init.sh <domain> [network], e.g. sh init.sh chat.example.com hq" >&2
  exit 2
}
[ $# -ge 1 ] && [ $# -le 2 ] || usage
domain=$1
network=${2:-hq}
# Each is one line (grep checks line by line): .env would get another.
nl='
'
case "$domain$network" in *"$nl"*) usage ;; esac

# A domain of lowercase letters, digits and hyphens, in two or more parts.
if ! printf '%s' "$domain" | grep -Eq '^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$'; then
  echo "init.sh: $domain isn't a domain (lowercase, e.g. chat.example.com)" >&2
  exit 2
fi
# As the console says a network's name is: 2 to 30 lowercase letters, digits
# and single hyphens, starting with a letter.
if [ ${#network} -lt 2 ] || [ ${#network} -gt 30 ] || ! printf '%s' "$network" | grep -Eq '^[a-z][a-z0-9]*(-[a-z0-9]+)*$'; then
  echo "init.sh: $network isn't a network's name: 2 to 30 lowercase letters, digits and single hyphens, starting with a letter" >&2
  exit 2
fi

# The release this is (VERSION, at the repo's root): the install runs its
# images until it's upgraded on purpose.
version=$(cat ../../VERSION 2>/dev/null) || version=
case "$version" in *"$nl"*) version= ;; esac
if ! printf '%s' "$version" | grep -Eq '^[0-9]{2}\.(0[1-9]|1[0-2])\.(0|[1-9][0-9]*)$'; then
  echo "init.sh: the repo's VERSION doesn't name a release (YY.0M.N, e.g. 26.10.0): get a release, or this repo as a whole" >&2
  exit 1
fi

# The secrets can't change once there's data made with them: never replaced.
if [ -e .env ]; then
  echo "init.sh: .env is already here, and its secrets are what this install's data was made with: it's left as it is" >&2
  exit 1
fi

# secret is 32 random bytes, as letters, digits, - and _.
secret() { head -c 32 /dev/urandom | base64 | tr -d '\n=' | tr '+/' '_-'; }

umask 077
tmp=$(mktemp .env.XXXXXX)
trap 'rm -f "$tmp"' EXIT
cat > "$tmp" <<EOF
# This install's settings, made by init.sh. The secrets below are what its
# data is made with: keep this file (and a copy with your backups) private,
# and never change them.

GUILDNET_DOMAIN=$domain
GUILDNET_NETWORK=$network

# The release this install runs (docs/self-host/operations.md#upgrading).
GUILDNET_VERSION=$version

GUILDNET_DB_PASSWORD=$(secret)
GUILDNET_PLATFORM_DB_PASSWORD=$(secret)
GUILDNET_RUNNER_DB_PASSWORD=$(secret)
GUILDNET_PLATFORM_KEY=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
GUILDNET_API_TOKEN=$(secret)
GUILDNET_ERGO_API_TOKEN=$(secret)
GUILDNET_CHAT_KEY=$(secret)
GUILDNET_ACCOUNT_SECRET=$(secret)
GUILDNET_OPER_PASSWORD=$(secret)

# Every setting is in docs/self-host/configuration.md.

# Email, through any mail server (docs/self-host/email.md). Without it,
# nobody gets invites or password resets by email.
# GUILDNET_SMTP_ADDR=smtp.example.com:587
# GUILDNET_SMTP_USERNAME=
# GUILDNET_SMTP_PASSWORD=
# GUILDNET_SMTP_SECURITY=starttls
# GUILDNET_MAIL_FROM=GuildNet <guildnet@example.com>
# GUILDNET_MAIL_REPLY_TO=

# A GitHub App of your own, for agents to work on your repos
# (docs/self-host/github.md).
# GUILDNET_GITHUB_PUBLIC_APP_ID=
# GUILDNET_GITHUB_PUBLIC_APP_SLUG=
# GUILDNET_GITHUB_PUBLIC_CLIENT_ID=
# GUILDNET_GITHUB_PUBLIC_CLIENT_SECRET=
# GUILDNET_GITHUB_PUBLIC_APP_KEY=

# Your own name and look (docs/self-host/brand.md); files go in brand/.
# GUILDNET_BRAND_NAME=
# GUILDNET_BRAND_TAGLINE=
# GUILDNET_BRAND_HELLO=
# GUILDNET_BRAND_SUPPORT=
# GUILDNET_BRAND_PRIVACY=
# GUILDNET_BRAND_SECURITY=
EOF
mv "$tmp" .env
trap - EXIT
# Readable by the console, which runs as a user of its own.
mkdir -p brand
chmod 755 brand

cat <<EOF
Wrote .env, with this install's secrets (not shown here).

Next:
  1. Point both of these at this machine (DNS A records: IPv4), and let
     ports 80, 443 and 6697 in:
       $domain
       $network.$domain
  2. Start it, with GuildNet $version's images:
       docker compose pull
       docker compose up -d
     (or build them from this repo instead: docker compose up -d --build)
  3. Get the setup code, then open https://$domain/ and enter it:
       docker compose logs platform | grep 'setup code'
EOF
