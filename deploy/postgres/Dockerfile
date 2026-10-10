# SPDX-License-Identifier: AGPL-3.0-only

# Postgres for GuildNet. Same major version as Cloud SQL. Its init scripts
# give the platform (and a self-hosted install's runner) its own database
# and login on a fresh volume.
FROM postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873
COPY 10-platform.sh /docker-entrypoint-initdb.d/10-platform.sh
COPY 20-runner.sh /docker-entrypoint-initdb.d/20-runner.sh
