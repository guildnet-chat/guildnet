# Connectors

Agents use Linear, Notion, Sentry and Atlassian through each service's own
MCP server, and any other MCP server the owner adds ([below](#servers-of-your-own)). Actions show as the account that was connected: use a shared or
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

## Which tools only read

The catalogue (`internal/connectors/connectors.go`) names each service's
tools that only read; every other tool asks first.

| Service | Run without asking |
| --- | --- |
| Linear | `get_*`, `list_*`, `search_documentation`, `extract_images` |
| Notion | `notion-search`, `notion-fetch`, `notion-get-*` |
| Sentry | `find_*`, `search_*`, `get_*`, `whoami`; `execute_sentry_tool` runs without asking only when the tool it names reads |
| Atlassian | none yet: every tool asks first |

## Servers of your own

Beside the catalogue, the owner adds any MCP server under **Add a server of
your own** on the Connectors page: a name, its `https://` address and,
unless it offers the MCP sign-in, a token (an API key).

- With a token, the token is tried first (the server must list its tools
  with it), then kept sealed like the rest. It's sent as
  `Authorization: Bearer <token>` and is bound to the server's address: one
  changed in the database doesn't open.
- Without one, the server's own sign-in is used, as for the catalogue's.
- Every tool asks a person first. People who may approve can set one tool
  at a time to run without asking.
- Disconnecting it forgets it. A network may add 10.

## Private servers

Services and servers of your own must be on the internet. To let networks
add an MCP server on the install's own network, name its host (or address)
in `GUILDNET_CONNECTOR_PRIVATE_HOSTS` on the console and the runner,
comma-separated.

## What agents see

A connector's results reach agents marked as untrusted content, not
instructions. A write through one connector shortly after reading from
another (or from a desk) asks a person first, whatever its policy.
