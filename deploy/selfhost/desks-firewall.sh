#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Keeps desks apart: on the desks' network (172.31.255.0/24) each desk
# reaches its proxy (egress, at .2) and nothing else, not another desk and
# not this machine. The network itself has no way out, and the proxy's own
# (172.31.254.0/24) goes only to public addresses.
#
#   sudo sh desks-firewall.sh            # sets the rules up now
#   sudo sh desks-firewall.sh install    # and whenever Docker starts
#
# Safe to run again: it rebuilds its own chains and adds each jump once.
set -eu
desks=172.31.255.0/24
proxy=172.31.255.2
outbound=172.31.254.0/24

if [ "${1:-}" = install ]; then
  dir=/usr/local/lib/guildnet
  mkdir -p "$dir"
  cp "$0" "$dir/desks-firewall.sh"
  # Traffic between containers on one Docker network goes through iptables
  # only with br_netfilter.
  echo br_netfilter > /etc/modules-load.d/guildnet-desks.conf
  printf 'net.bridge.bridge-nf-call-iptables = 1\n' > /etc/sysctl.d/90-guildnet-desks.conf
  cat > /etc/systemd/system/guildnet-desks-firewall.service <<EOF
[Unit]
Description=Keep GuildNet's desks apart from each other and this machine
After=docker.service
Requires=docker.service
PartOf=docker.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh $dir/desks-firewall.sh
[Install]
WantedBy=docker.service
EOF
  systemctl daemon-reload
  systemctl enable guildnet-desks-firewall.service
  systemctl restart guildnet-desks-firewall.service
  echo "desks firewall: installed, and set up whenever Docker starts"
  exit 0
fi

if [ -w /proc/sys/net/bridge/bridge-nf-call-iptables ] || modprobe br_netfilter 2>/dev/null; then
  sysctl -q -w net.bridge.bridge-nf-call-iptables=1 2>/dev/null || true
fi

# These rules are IPv4's, and desks get no IPv6 (their network says so): a
# desks' network that has it anyway is refused, not kept half apart.
if command -v docker >/dev/null 2>&1 &&
   [ "$(docker network inspect guildnet-desks --format '{{.EnableIPv6}}' 2>/dev/null)" = true ]; then
  echo "desks firewall: the desks' network (guildnet-desks) has IPv6, which these rules don't cover: remake it without (enable_ipv6: false)" >&2
  exit 1
fi

# chain makes an empty chain, or empties it.
chain() { iptables -N "$1" 2>/dev/null || iptables -F "$1"; }
# once adds a rule unless it's there.
once() { iptables -C "$@" 2>/dev/null || iptables -I "$@"; }

# Desk to desk (and to the proxy): only the proxy's port, and replies.
chain GUILDNET-DESKS
iptables -A GUILDNET-DESKS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A GUILDNET-DESKS -d "$proxy/32" -p tcp --dport 3128 -j RETURN
iptables -A GUILDNET-DESKS -j DROP
once DOCKER-USER -s "$desks" -d "$desks" -j GUILDNET-DESKS

# Desks reaching this machine (SSH, anything listening): only replies to
# connections it started.
chain GUILDNET-DESKS-HOST
iptables -A GUILDNET-DESKS-HOST -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A GUILDNET-DESKS-HOST -j REJECT
once INPUT -s "$desks" -j GUILDNET-DESKS-HOST

# The proxy's way out (its own network): lookups (Docker makes them from the
# proxy's network, to this machine's resolver, which may be on a private or
# link-local address), then the internet only. Not the cloud's metadata
# server (this VM's service account's tokens), nor the private networks,
# whatever the proxy's own checks would let by.
chain GUILDNET-DESKS-OUT
iptables -A GUILDNET-DESKS-OUT -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A GUILDNET-DESKS-OUT -p udp --dport 53 -j RETURN
iptables -A GUILDNET-DESKS-OUT -p tcp --dport 53 -j RETURN
for net in 169.254.0.0/16 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10; do
  iptables -A GUILDNET-DESKS-OUT -d "$net" -j REJECT
done
once DOCKER-USER -s "$outbound" -j GUILDNET-DESKS-OUT

echo "desks firewall: desks reach only their proxy, and it only the internet"
