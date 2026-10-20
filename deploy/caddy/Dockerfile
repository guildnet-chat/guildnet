# SPDX-License-Identifier: AGPL-3.0-only

# A self-hosted install's edge: Caddy with the layer4 app (caddy-l4), for
# IRC over TLS, and its config built in. Its settings (the domain and the
# network's name) come from the environment.
FROM caddy:2-builder@sha256:b25f47453fa02f7e66c0828b4b1343658808b9000950b6825c4a1cb0c988f5f8 AS build
RUN xcaddy build --with github.com/mholt/caddy-l4@v0.1.2

FROM caddy:2@sha256:f2a1290d0463aad60660d4ec134943f183ee2a5f6c3eb7bf32dd984f2f020772
COPY --from=build /usr/bin/caddy /usr/bin/caddy
# No file capability (the base image's binding of 80 and 443): with every
# capability dropped, Linux refuses to run a binary that has one. The stack
# lets it bind them as its own user instead (compose.yml).
RUN setcap -r /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
