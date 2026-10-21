#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Makes a desks host's settings, for a console that runs elsewhere. Run
# once, on the desks host, with its private address (where the console and
# the runner reach it):
#
#   sh init.sh 10.0.0.5                     # needs gVisor and the firewall
#   sh init.sh 10.0.0.5 --without-gvisor    # each desk an ordinary container
#   sh init.sh 10.0.0.5 --without-firewall  # only to try desks out
#
# It writes, each readable only by whoever runs it:
#   .env          this host's settings: the release, its address, and
#                 sandboxd's half of the key pair desks are reached with
#   console.env   the console's settings for it: the console's half (which
#                 signs the runner's tokens), and where sandboxd is. Add them
#                 to the console's settings, then delete this file
#   sandboxd.csr  a request for sandboxd's certificate, for the console to
#                 sign (its key stays here)
# docs/self-host/desks-host.md says what's next.
set -eu
cd "$(dirname "$0")"

usage() {
  echo "usage: sh init.sh <private address> [--without-gvisor] [--without-firewall]" >&2
  exit 2
}
[ $# -ge 1 ] || usage
address=$1
shift
gvisor=yes
firewall=yes
for arg in "$@"; do
  case "$arg" in
    --without-gvisor) gvisor=no ;;
    --without-firewall) firewall=no ;;
    *) usage ;;
  esac
done

# An IPv4 address on a private network: sandboxd has Docker's socket, and is
# reached only there.
if ! printf '%s' "$address" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$'; then
  echo "init.sh: $address isn't an IPv4 address (this host's private address, e.g. 10.0.0.5)" >&2
  exit 2
fi
case "$address" in
  10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*) ;;
  *) echo "init.sh: $address isn't a private address (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16): sandboxd is reached only on a private network" >&2; exit 2 ;;
esac

version=$(cat ../../VERSION 2>/dev/null) || version=
if ! printf '%s' "$version" | grep -Eq '^[0-9]{2}\.(0[1-9]|1[0-2])\.(0|[1-9][0-9]*)$'; then
  echo "init.sh: the repo's VERSION doesn't name a release (YY.0M.N, e.g. 26.10.0): get a release, or this repo as a whole" >&2
  exit 1
fi

# The key pair can't change while desks run: never replaced.
if [ -e .env ]; then
  echo "init.sh: .env is already here, with this desks host's key: it's left as it is" >&2
  exit 1
fi

if [ "$gvisor" = yes ] && ! docker info --format '{{json .Runtimes}}' | grep -q '"runsc"'; then
  cat >&2 <<'EOF'
init.sh: Docker has no gVisor (runsc) runtime, which gives each desk a
kernel of its own. Install it as https://gvisor.dev/docs/user_guide/install/
says (sudo runsc install, then restart Docker), and run this again, or add
--without-gvisor. Nothing changed.
EOF
  exit 1
fi
if [ "$firewall" = yes ] && ! { command -v systemctl >/dev/null && systemctl is-active --quiet guildnet-desks-firewall; }; then
  cat >&2 <<'EOF'
init.sh: the desks' firewall isn't set up. It keeps desks from each other
and from this machine. Set it up (as root; again whenever Docker starts):

  sudo sh ../selfhost/desks-firewall.sh install

and run this again, or add --without-firewall. Nothing changed.
EOF
  exit 1
fi

umask 077
# Before .env is there, Compose takes what it needs from the environment.
export GUILDNET_VERSION="$version" GUILDNET_DESKS_HOST_ADDRESS="$address"
keys=$(docker compose run --rm --no-deps -T tls keygen)
case "$keys" in
  *GUILDNET_SANDBOX_KEY=*GUILDNET_SANDBOX_PUBLIC_KEY=*) ;;
  *) echo "init.sh: sandboxd didn't make a key pair; nothing changed" >&2; exit 1 ;;
esac

env=$(mktemp .env.XXXXXX)
console=$(mktemp console.env.XXXXXX)
trap 'rm -f "$env" "$console"' EXIT
{
  echo "# This desks host's settings, made by init.sh. The key is what the"
  echo "# console's tokens are checked with: never change it while desks run."
  echo
  echo "GUILDNET_VERSION=$version"
  echo "GUILDNET_DESKS_HOST_ADDRESS=$address"
  printf '%s\n' "$keys" | grep '^GUILDNET_SANDBOX_PUBLIC_KEY='
  if [ "$gvisor" = no ]; then
    echo "# Without gVisor: each desk shares this machine's kernel."
    echo "GUILDNET_DESK_RUNTIME=runc"
  fi
  if [ "$firewall" = no ]; then
    echo "# Without the desks' firewall: desks can reach each other and this"
    echo "# machine. sandboxd doesn't check for it."
    echo "GUILDNET_DESK_FIREWALL=off"
  fi
  echo
  echo "# Every setting is in docs/self-host/configuration.md#desks."
} > "$env"
{
  echo "# The console's settings for the desks host at $address, made by its"
  echo "# init.sh: add them to the console's, then delete this file."
  printf '%s\n' "$keys" | grep '^GUILDNET_SANDBOX_KEY='
  echo "GUILDNET_SANDBOX_URL=https://$address:8443"
  echo "GUILDNET_SANDBOX_NAMES=$address"
} > "$console"
mv "$env" .env
mv "$console" console.env
trap - EXIT

# The request for sandboxd's certificate; its key stays in the tls volume.
docker compose run --rm --no-deps -T tls tls-request > sandboxd.csr

cat <<EOF
Wrote .env and console.env (the console's key is in console.env, not
shown here), and sandboxd.csr, a request for sandboxd's certificate.

Next:
  1. Add console.env's settings to the console's (its .env, or its
     secret), restart the console, and delete console.env here.
  2. Have the console sign sandboxd.csr, e.g. on a Compose install:
       docker compose exec -T platform /platform sandbox-cert < sandboxd.csr > sandboxd.crt
     and bring sandboxd.crt here.
  3. Install it, and start the desks host:
       docker compose run --rm -T tls tls-install < sandboxd.crt
       docker compose pull
       docker compose up -d
EOF
