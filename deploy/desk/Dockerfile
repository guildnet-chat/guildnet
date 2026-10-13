# SPDX-License-Identifier: AGPL-3.0-only

# A desk: an agent's workspace, run by sandboxd (gVisor on the sandbox VM).
# Go, Node and Python to build and test with, git, and ripgrep for the grep
# and glob tools. The agent works as "desk" (uid 1000) in /workspace; its
# home keeps caches (Go modules, npm, pip) between wake-ups, and its startup
# files are put back as below each time it's readied. Commands run in bash
# reading no startup files, so PATH is the ENV's: the image's folders first,
# the home's (go install's, pip --user's) last, so nothing left in the home
# stands in for a system command. The internet is reached only through the
# proxy sandboxd sets. xfsprogs is for sandboxd, which sets networks'
# storage limits from a short-lived container of this image (a desk can't:
# it has no capabilities).
FROM golang:1.27-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
RUN apt-get update && apt-get install -y --no-install-recommends \
        ripgrep python3 python3-venv python3-pip nodejs npm jq less procps xfsprogs \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home --home-dir /home/desk --shell /bin/bash desk \
    && mkdir -p /workspace && chown desk:desk /workspace
ENV GOPATH=/home/desk/go \
    GOTOOLCHAIN=auto \
    PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/home/desk/go/bin:/home/desk/.local/bin \
    GIT_TERMINAL_PROMPT=0
USER desk
WORKDIR /workspace
RUN git config --global init.defaultBranch main \
    && git config --global user.name "GuildNet desk" \
    && git config --global user.email "desk@guildnet.invalid"
CMD ["sleep", "infinity"]
