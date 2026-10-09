# SPDX-License-Identifier: AGPL-3.0-only

# GuildNet platform (web app and consoles). Build context is a module's
# root: this repo's builds the open console; a build with more plugged in
# (GuildNet's own) builds its own cmd/platform the same way. The chat app
# comes from the guildnet-chat repo as the "chat-src" build context, e.g.
# docker build --build-context chat-src=../guildnet-chat ... It's AGPL and
# stays a separate program: the platform only serves its built files.
FROM node:25-alpine@sha256:bdf2cca6fe3dabd014ea60163eca3f0f7015fbd5c7ee1b0e9ccb4ced6eb02ef4 AS chat
WORKDIR /chat
COPY --from=chat-src package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY --from=chat-src . .
RUN npm run build

FROM golang:1.27-alpine@sha256:738d1cf061836894ff6bb8c33881080ac66de8cf0586615012a0c8f592649cfa AS build
WORKDIR /src
COPY . .
# Which build this is, in reports of faults, and which guildnet-chat commit
# the chat app is, for its Source link.
ARG GUILDNET_RELEASE
ARG GUILDNET_CHAT_COMMIT
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/guildnet-chat/guildnet/monitor.release=${GUILDNET_RELEASE} -X main.chatCommit=${GUILDNET_CHAT_COMMIT}" -o /out/platform ./cmd/platform && \
    mkdir -p /out/broker/own /out/broker/shared /out/broker/sandbox

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/platform /platform
COPY --from=chat /chat/dist /chat
# Where a self-hosted console keeps its broker's CA, and shares the CA's
# certificate: owned by nonroot, so new volumes there are too.
COPY --from=build --chown=nonroot:nonroot /out/broker /var/lib/guildnet-broker
ENV GUILDNET_CHAT_DIR=/chat
ENTRYPOINT ["/platform"]
