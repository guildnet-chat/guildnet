# SPDX-License-Identifier: AGPL-3.0-only

# Ergo with the GuildNet config and welcome baked in (avoids bind-mounting
# host files). start.sh fills in the welcome for the network it runs.
#
# Ergo is built from source, at its release, with two patches:
# authoritative-auth-script.patch makes the auth-script's answer final, and
# people can't change their own credentials or unregister over IRC (the
# console keeps people's passwords; guildnet-ircauth, in ircauth/, asks it);
# motd-file-only.patch sends a welcome only from a file, never a link (the
# runner, which shares /welcome, could leave one to Ergo's database). Its
# golang.org/x/crypto is moved up to a release with the fixes Ergo's own
# lacks (the weekly scan finds them).
FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
RUN apk add --no-cache git
ARG ERGO_VERSION=v2.19.1
ARG ERGO_COMMIT=63c743a70644f0f19109508ade8580bf37f7d23d
RUN git clone --quiet --depth 1 --branch "$ERGO_VERSION" https://github.com/ergochat/ergo.git /ergo \
    && test "$(git -C /ergo rev-parse HEAD)" = "$ERGO_COMMIT"
COPY authoritative-auth-script.patch motd-file-only.patch /ergo/
WORKDIR /ergo
RUN git apply authoritative-auth-script.patch motd-file-only.patch \
    && go get golang.org/x/crypto@v0.58.0 && go mod vendor \
    && CGO_ENABLED=0 go build -trimpath -tags "i18n mysql postgresql sqlite" \
        -ldflags "-X main.commit=$ERGO_COMMIT -X main.version=$ERGO_VERSION-guildnet" -o /out/ergo .
COPY ircauth/main.go /ircauth/main.go
WORKDIR /ircauth
RUN CGO_ENABLED=0 go build -trimpath -o /out/guildnet-ircauth main.go

# The upstream image's layout (/ircd-bin, /ircd), on Alpine, with the
# release's languages. Not the upstream image itself: it's rebuilt only with
# Ergo's releases, and its default.yaml's example key looks like a leaked
# one to scans.
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
WORKDIR /ircd
VOLUME /ircd
EXPOSE 6667 6697
COPY --from=build /ergo/languages /ircd-bin/languages/
COPY --from=build /out/ergo /ircd-bin/ergo
COPY --from=build /out/guildnet-ircauth /ircd-bin/guildnet-ircauth
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
