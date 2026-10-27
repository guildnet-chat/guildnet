# SPDX-License-Identifier: AGPL-3.0-only

# A self-hosted install's edge: Caddy with the layer4 app (caddy-l4), for
# IRC over TLS, and its config built in. Its settings (the domain and the
# network's name) come from the environment.
FROM caddy:2-builder@sha256:c2fc374fd352439d76e1777b28d97d9f6d9c1dd86116db9227460374d46097d5 AS build
# golang.org/x/net is moved up to a release with fixes Caddy's lacks.
RUN xcaddy build --with github.com/mholt/caddy-l4@v0.1.2 --replace golang.org/x/net=golang.org/x/net@v0.61.0

FROM caddy:2@sha256:504a44d7d74bd2bc28a66afc4774912fc21223ca8a67734af1c54ab2f999b489
RUN apk upgrade --no-cache
COPY --from=build /usr/bin/caddy /usr/bin/caddy
# No file capability (the base image's binding of 80 and 443): with every
# capability dropped, Linux refuses to run a binary that has one. The stack
# lets it bind them as its own user instead (compose.yml).
RUN setcap -r /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
