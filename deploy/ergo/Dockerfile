# SPDX-License-Identifier: AGPL-3.0-only

# Ergo with the GuildNet config and welcome baked in (avoids bind-mounting
# host files). start.sh fills in the welcome for the network it runs.
FROM ghcr.io/ergochat/ergo:stable@sha256:72db5b93b437ea0512fb79d9d4d035f0916ccba07ed736dec19e8d5b5a73f972
COPY ircd.yaml /ircd-bin/guildnet.yaml
COPY guildnet.motd /ircd-bin/guildnet.motd
COPY start.sh /ircd-bin/start.sh
# Customers' networks run Ergo as nobody (the host sets the user); a new
# volume at /ircd takes this ownership. The local network's older volume is
# root's, and it keeps running as root.
RUN chown nobody:nobody /ircd
# The welcome, in a volume shared with the runner (another user), which a
# new volume here takes this ownership and these permissions from.
COPY guildnet.motd /welcome/guildnet.motd
RUN chown -R nobody:nobody /welcome && chmod 0777 /welcome
ENTRYPOINT ["/bin/sh", "/ircd-bin/start.sh"]
