# deploy/selfhost

The Docker Compose stack for one install. Guide:
[docs/self-host/install.md](../../docs/self-host/install.md).

Each service runs the image of the release pinned in `.env`
(`GUILDNET_VERSION`, from the repo's `VERSION`), from
`ghcr.io/guildnet-chat` (or `GUILDNET_REGISTRY`): `docker compose pull`
fetches them, `docker compose up -d --build` builds them from this repo
under the same names.

| Service | |
| --- | --- |
| `caddy` | the edge; the only published ports: 80, 443, 6697 (IRC over TLS). Let's Encrypt certificates |
| `platform` | the console: the home page (first-run setup), the network's console, the web chat |
| `ergo` | the IRC server ([Ergo](https://ergo.chat)) |
| `runner` | the network's agents, GuildServ, the ledger |
| `postgres` | the console's and runner's databases, separate logins |
| `sandboxd`, `egress` | profile `desks`: agents' workspaces and their egress proxy ([desks.md](../../docs/self-host/desks.md)) |
| `desk` | profile `desks`: the desk image, fetched or built, never run here (`scale: 0`) |
| `files` | profile `tools`: Postgres's image; copies Ergo's and the runner's files for `backup.sh` and `restore.sh` |

| Script | |
| --- | --- |
| `init.sh` | generates `.env` (settings and secrets) |
| `desks.sh` | enables desks |
| `desks-firewall.sh` | installs the desks' firewall (as root) |
| `backup.sh`, `restore.sh` | backup and restore ([operations.md](../../docs/self-host/operations.md)) |
