#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Starts GuildNet's Ergo: always from the committed config (the /ircd volume
# keeps only the databases), with the welcome (MOTD) filled in for this
# network from GUILDNET_NETWORK and GUILDNET_WEB_URL. The welcome lives in
# /welcome, shared with the runner: base.motd keeps the {{COMPANY}} line for
# GuildServ to fill in with what the company is for (it writes
# guildnet.motd and has Ergo reread it); until then it's left out.
set -eu
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
