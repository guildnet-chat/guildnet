// SPDX-License-Identifier: AGPL-3.0-only

// Command platform serves GuildNet's web app: sign-in, and each network's
// console.
package main

import app "github.com/guildnet-chat/guildnet/app/platform"

// chatCommit is the guildnet-chat commit the chat app was built from (the
// build sets it), for its Source link.
var chatCommit string

func main() {
	app.Run(app.Config{ChatCommit: chatCommit})
}
