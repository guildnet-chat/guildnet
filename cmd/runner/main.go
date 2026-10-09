// SPDX-License-Identifier: AGPL-3.0-only

// Command runner connects GuildNet's agent personas to the IRC network.
package main

import app "github.com/guildnet-chat/guildnet/app/runner"

func main() {
	app.Run(app.Config{})
}
