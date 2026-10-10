#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Makes NOTICE: what's written by hand in .github/notice/head.txt, then the
# Go modules GuildNet is built with and their licences, from go-licenses
# (go install github.com/google/go-licenses/v2@v2.0.1). CI runs it and
# fails if NOTICE changes.
#
# segmentio/asm is MIT-0, which go-licenses doesn't know: it's listed in
# head.txt by hand.
set -eu
cd "$(dirname "$0")/.."
{
	cat .github/notice/head.txt
	GOOS=linux GOARCH=amd64 go-licenses report ./... \
		--ignore github.com/guildnet-chat/guildnet \
		--ignore github.com/segmentio/asm \
		--template .github/notice/go.tpl 2>/dev/null |
		tr '\\' / | # on Windows, go-licenses joins URLs with backslashes
		LC_ALL=C sort -u
} > NOTICE
