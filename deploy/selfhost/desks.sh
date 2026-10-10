#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Turns desks on: each agent can have a workspace of its own (a container,
# under gVisor, whose only way out is a proxy to GitHub and the package
# registries). Run once, after init.sh:
#
#   sh desks.sh                    # needs gVisor (runsc) set up in Docker,
#                                  # and the desks' firewall set up
#   sh desks.sh --without-gvisor   # each desk an ordinary container instead
#   sh desks.sh --without-firewall # desks can reach each other and this
#                                  # machine: only to try GuildNet out
#
# It makes the key pair desks are reached with (the console's half signs the
# runner's tokens; sandboxd's checks them) into .env, never shown, and turns
# the desks profile on there. Running it again changes nothing.
set -eu
cd "$(dirname "$0")"

gvisor=yes
firewall=yes
for arg in "$@"; do
  case "$arg" in
    --without-gvisor) gvisor=no ;;
    --without-firewall) firewall=no ;;
    *) echo "usage: sh desks.sh [--without-gvisor] [--without-firewall]" >&2; exit 2 ;;
  esac
done

if [ ! -e .env ]; then
  echo "desks.sh: there's no .env here yet: run init.sh first" >&2
  exit 1
fi
if grep -q '^GUILDNET_SANDBOX_KEY=' .env; then
  echo "Desks are already on: nothing changed."
  exit 0
fi

if [ "$gvisor" = yes ] && ! docker info --format '{{json .Runtimes}}' | grep -q '"runsc"'; then
  cat >&2 <<'EOF'
desks.sh: Docker has no gVisor (runsc) runtime, which gives each desk a
kernel of its own. Install it as https://gvisor.dev/docs/user_guide/install/
says, with Docker set up to use it (sudo runsc install, then restart Docker),
and run this again. Or run desks.sh --without-gvisor to use Docker's own
runtime (each desk then shares this machine's kernel). Nothing changed.
EOF
  exit 1
fi

if [ "$firewall" = yes ] && ! { command -v systemctl >/dev/null && systemctl is-active --quiet guildnet-desks-firewall; }; then
  cat >&2 <<'EOF'
desks.sh: the desks' firewall isn't set up. It's what keeps desks from each
other and from this machine: without it, an agent's desk can reach any
other desk, and anything this machine listens on. Set it up (as root; it's
set up again whenever Docker starts):

  sudo sh desks-firewall.sh install

and run this again. Or run desks.sh --without-firewall, only to try
GuildNet out. Nothing changed.
EOF
  exit 1
fi

# The key pair, from sandboxd itself (its image fetched or built first, if
# it isn't here yet): straight into .env, never shown. Only the keys' lines
# are kept from what it prints.
keys=$(docker compose run --rm --no-deps -T sandboxd keygen)
case "$keys" in
  *GUILDNET_SANDBOX_KEY=*GUILDNET_SANDBOX_PUBLIC_KEY=*) ;;
  *) echo "desks.sh: sandboxd didn't make a key pair; nothing changed" >&2; exit 1 ;;
esac

umask 077
tmp=$(mktemp .env.XXXXXX)
trap 'rm -f "$tmp"' EXIT
{
  cat .env
  echo
  echo "# Desks, turned on by desks.sh. The key pair desks are reached with:"
  echo "# never change it while desks run."
  printf '%s\n' "$keys" | grep -E '^GUILDNET_SANDBOX_(PUBLIC_)?KEY='
  echo "GUILDNET_SANDBOX_URL=https://sandboxd:8443"
  if [ "$gvisor" = no ]; then
    echo "# Without gVisor: each desk shares this machine's kernel."
    echo "GUILDNET_DESK_RUNTIME=runc"
  fi
  echo "COMPOSE_PROFILES=desks"
} > "$tmp"
mv "$tmp" .env
trap - EXIT

cat <<'EOF'
Desks are on (their keys are in .env, not shown here).

Next, fetch the desk image, and start sandboxd and egress:
  docker compose pull
  docker compose up -d
(or, building from this repo, docker compose up -d --build: the desk image
takes several minutes)
EOF
