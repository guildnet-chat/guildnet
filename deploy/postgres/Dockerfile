# SPDX-License-Identifier: AGPL-3.0-only

# Postgres for GuildNet. Same major version as Cloud SQL. Its init scripts
# give the platform (and a self-hosted install's runner) its own database
# and login on a fresh volume.
FROM postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
# Alpine's packages up to date between the base image's rebuilds, and its
# entrypoint drops root with su-exec (the same arguments, in C) in place of
# gosu, which the base image ships built with an old Go (removed, not just
# replaced, so scanners don't find it in the base's layer).
RUN apk upgrade --no-cache && apk add --no-cache su-exec \
    && rm /usr/local/bin/gosu && mkdir -p /usr/local/sbin && ln -s /sbin/su-exec /usr/local/sbin/gosu
COPY 10-platform.sh /docker-entrypoint-initdb.d/10-platform.sh
COPY 20-runner.sh /docker-entrypoint-initdb.d/20-runner.sh
