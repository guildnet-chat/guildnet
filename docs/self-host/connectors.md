# Connectors

Agents use Linear, Notion, Sentry and Atlassian through each service's own
MCP server. Actions show as the account that was connected: use a shared or
bot account where the service allows it.

| Behaviour | |
| --- | --- |
| Reads | run without asking |
| Writes | ask a person first, unless set to run without asking |
| Tool definitions | pinned when approved; a changed or new definition is held until approved again |
| Credentials | stored in the console, sealed with `GUILDNET_PLATFORM_KEY`; the runner gets a short-lived access token per call from the broker; refresh tokens never leave the console |

## Requirements

The Compose stack meets these. When running the programs another way:

| Requirement | Setting |
| --- | --- |
| The console on its own domain over HTTPS | `GUILDNET_SECURE_COOKIES=on`. OAuth redirects go to `https://<domain>/oauth/callback` (the home page's domain, not the network's). |
| The broker, and the runner pointed at it | `GUILDNET_GITHUB_BROKER_LISTEN` on the console; `GUILDNET_GITHUB_BROKER` and `GUILDNET_NETWORK` on the runner ([github.md](github.md#running-the-programs-another-way); the same broker serves connectors with or without a GitHub App) |
| A database | `GUILDNET_PLATFORM_DATABASE_URL`; without one, connections are lost on restart |

No registration is needed. Services that accept client ID metadata documents
identify GuildNet by `https://<domain>/oauth/client-metadata.json`; others
are registered with automatically (dynamic client registration), once per
service.

## Connecting

As the owner, open **Connectors** in the console and **Connect** the
service, then sign in and authorize at the service. Its tools appear on the
same page within a few minutes; people who may approve set each tool's
policy there (allow, ask, deny).

When a service revokes access, its tools are disabled and the page offers
**Reconnect**.
