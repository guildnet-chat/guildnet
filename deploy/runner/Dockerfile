# SPDX-License-Identifier: AGPL-3.0-only

# GuildNet runner. Build context is a module's root: this repo's builds the
# open runner; a build with more plugged in (GuildNet's own) builds its own
# cmd/runner the same way, with OPEN saying where this repo is in it.
FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY . .
# One package at a time: the model providers' SDKs are each big to compile,
# and together they outgrow a small Docker's memory (2 GB). The build cache
# keeps rebuilds quick.
# Which build this is, in reports of faults.
ARG GUILDNET_RELEASE
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -p 1 -trimpath -ldflags "-X github.com/guildnet-chat/guildnet/monitor.release=${GUILDNET_RELEASE}" -o /out/runner ./cmd/runner && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG OPEN=.
WORKDIR /app
COPY --from=build /out/runner /app/runner
COPY ${OPEN}/guildnet.yaml /app/guildnet.yaml
COPY ${OPEN}/personas /app/personas
# What new networks start from (GUILDNET_CONFIG picks one).
COPY ${OPEN}/templates /app/templates
# Owned by nonroot so the ledger can write to the data volume.
COPY --from=build --chown=nonroot:nonroot /out/data /app/data
# Starts from /app/guildnet.yaml, or the template GUILDNET_CONFIG names.
ENTRYPOINT ["/app/runner"]
