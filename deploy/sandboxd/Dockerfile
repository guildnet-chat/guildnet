# SPDX-License-Identifier: AGPL-3.0-only

# sandboxd: keeps desks of GuildNet's own (serve), and is their only way out
# (proxy). Build context is this repo's root. serve drives Docker through the
# mounted socket, so on the sandbox VM nothing else runs beside it.
FROM golang:1.27-alpine@sha256:738d1cf061836894ff6bb8c33881080ac66de8cf0586615012a0c8f592649cfa AS build
WORKDIR /src
COPY . .
# Which build this is, in reports of faults.
ARG GUILDNET_RELEASE
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/guildnet-chat/guildnet/monitor.release=${GUILDNET_RELEASE}" -o /out/sandboxd ./cmd/sandboxd

FROM docker:28-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d
COPY --from=build /out/sandboxd /usr/local/bin/sandboxd
ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["serve"]
