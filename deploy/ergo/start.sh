#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Starts GuildNet's Ergo: always from the committed config (the /ircd volume
# keeps only the databases), with the welcome (MOTD) filled in for this
# network from GUILDNET_NETWORK and GUILDNET_WEB_URL. The welcome lives in
# /welcome, shared with the runner: base.motd keeps the {{COMPANY}} line for
# GuildServ to fill in with what the company is for (it writes
# guildnet.motd and has Ergo reread it); until then it's left out.
set -eu
# The image's own config lets nobody in; a deployment gives Ergo what makes
# it usable. Missing any of that, it would start a server nobody can use (or
# one that can't ask the console about people's logins): say so instead.
refuse() {
  echo "start.sh: refusing to start: $*" >&2
  exit 1
}
# set NAME: NAME is set and not empty.
set_() { eval "[ -n \"\${$1:-}\" ]"; }
# A JSON list with at least one string in it that isn't empty.
printf '%s' "${ERGO__API__BEARER_TOKENS:-}" | grep -q '"[^"][^"]*"' ||
  refuse "ERGO__API__BEARER_TOKENS is missing or empty (the console's and the runner's way in)"
set_ GUILDNET_OPER_PASSWORD || set_ ERGO__OPERS__GUILDSERV__PASSWORD ||
  refuse "GuildServ's operator password is missing (GUILDNET_OPER_PASSWORD, or its hash in ERGO__OPERS__GUILDSERV__PASSWORD)"
if [ "${ERGO__ACCOUNTS__JWT_AUTH__ENABLED:-}" = true ]; then
  printf '%s' "${ERGO__ACCOUNTS__JWT_AUTH__TOKENS:-}" | grep -q '"key": *"[^"]' ||
    refuse "the web chat is on but ERGO__ACCOUNTS__JWT_AUTH__TOKENS has no key"
fi
if set_ GUILDNET_IRC_AUTH_URL || set_ GUILDNET_ERGO_TOKEN; then
  set_ GUILDNET_IRC_AUTH_URL || refuse "GUILDNET_ERGO_TOKEN is set without GUILDNET_IRC_AUTH_URL (the console to ask about logins)"
  set_ GUILDNET_ERGO_TOKEN || refuse "GUILDNET_IRC_AUTH_URL is set without GUILDNET_ERGO_TOKEN (Ergo's token for the console)"
  case "$GUILDNET_IRC_AUTH_URL" in
    https://*) set_ GUILDNET_BROKER_CA || set_ GUILDNET_BROKER_CA_FILE ||
      refuse "GUILDNET_IRC_AUTH_URL is https but there's no GUILDNET_BROKER_CA or GUILDNET_BROKER_CA_FILE to check the console's certificate" ;;
  esac
fi
cp /ircd-bin/guildnet.yaml /ircd/ircd.yaml
network="${GUILDNET_NETWORK:-this network}"
web="${GUILDNET_WEB_URL:-your network's own address}"
# Each written to a new file of its own (never through anything left in the
# shared directory), made readable to the runner, and moved into place.
put() {
  tmp=$(mktemp /welcome/.start.XXXXXX)
  cat > "$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "/welcome/$1"
}
sed -e "s|{{NETWORK}}|${network}|g" -e "s|{{WEB}}|${web}|g" /ircd-bin/guildnet.motd | put base.motd
grep -v '^{{COMPANY}}$' /welcome/base.motd | put guildnet.motd
# A self-hosted install gives GuildServ's operator password as the runner
# has it (GUILDNET_OPER_PASSWORD), and its hash is made here; on
# guildnet.chat the host gives the hash.
if [ -n "${GUILDNET_OPER_PASSWORD:-}" ] && [ -z "${ERGO__OPERS__GUILDSERV__PASSWORD:-}" ]; then
  ERGO__OPERS__GUILDSERV__PASSWORD=$(printf '%s\n' "$GUILDNET_OPER_PASSWORD" | /ircd-bin/ergo genpasswd)
  export ERGO__OPERS__GUILDSERV__PASSWORD
fi
unset GUILDNET_OPER_PASSWORD
exec /ircd-bin/ergo run --conf /ircd/ircd.yaml
