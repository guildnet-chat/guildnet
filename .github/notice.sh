#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Makes NOTICE: what's written by hand in .github/notice/head.txt, then the
# Go modules GuildNet is built with and their licences, from go-licenses
# (go install github.com/google/go-licenses/v2@v2.0.1). CI runs it and
# fails if NOTICE changes.
#
# go-licenses checks each licence's address over the network, and gives
# "Unknown" when a check fails: it's asked again (up to 3 times) rather
# than writing that down.
#
# segmentio/asm is MIT-0, which go-licenses doesn't know: it's listed in
# head.txt by hand.
set -eu
cd "$(dirname "$0")/.."
report=$(mktemp)
trap 'rm -f "$report"' EXIT
for try in 1 2 3; do
	GOOS=linux GOARCH=amd64 go-licenses report ./... \
		--ignore github.com/guildnet-chat/guildnet \
		--ignore github.com/segmentio/asm \
		--template .github/notice/go.tpl 2>/dev/null |
		tr '\\' / | # on Windows, go-licenses joins URLs with backslashes
		LC_ALL=C sort -u > "$report"
	# yaml.v3's repository (go-yaml, archived) is often not found: its
	# licence's address is always its pkg.go.dev page, found or not.
	sed -E 's|^(gopkg\.in/yaml\.v3 (v[^:]+): MIT), .*$|\1, https://pkg.go.dev/gopkg.in/yaml.v3@\2?tab=licenses|' "$report" > "$report.fixed"
	mv -f "$report.fixed" "$report"
	grep -q ', Unknown$' "$report" || break
	if [ "$try" = 3 ]; then
		echo "notice.sh: go-licenses couldn't find these licences' addresses:" >&2
		grep ', Unknown$' "$report" >&2
		exit 1
	fi
	sleep 5
done
{
	cat .github/notice/head.txt
	cat "$report"
} > NOTICE
