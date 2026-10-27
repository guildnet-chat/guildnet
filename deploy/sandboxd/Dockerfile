# SPDX-License-Identifier: AGPL-3.0-only

# sandboxd: keeps desks of GuildNet's own (serve), and is their only way out
# (proxy). Build context is this repo's root. serve drives Docker through the
# mounted socket, so on the sandbox VM nothing else runs beside it.
FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY . .
# Which build this is, in reports of faults.
ARG GUILDNET_RELEASE
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/guildnet-chat/guildnet/monitor.release=${GUILDNET_RELEASE}" -o /out/sandboxd ./cmd/sandboxd

FROM docker:29-cli@sha256:5e6baf67325a79f64ed3bed6863954d5e05d0f3d4e57cbdb14809346814bbfdb
# Only the docker command itself: not Compose or Buildx, which sandboxd
# never runs. Alpine's packages up to date between the base's rebuilds.
RUN rm -rf /usr/local/libexec/docker/cli-plugins && apk upgrade --no-cache
COPY --from=build /out/sandboxd /usr/local/bin/sandboxd
ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["serve"]
