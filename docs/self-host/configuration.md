# Configuration

Settings are environment variables in `.env`, beside `compose.yml` in
`deploy/selfhost`. After editing, run `docker compose up -d`; services whose
settings changed are recreated.

| Section | Source |
| --- | --- |
| [Operator settings](#operator-settings) | set by you in `.env` |
| [Generated](#generated) | written by `init.sh` and `desks.sh`; never change them |
| [Set by the stack](#set-by-the-stack) | `compose.yml`; needed only when running the programs another way |
| [Not for a self-hosted install](#not-for-a-self-hosted-install) | guildnet.chat's own, and development |

Ergo also reads its own settings from the environment as `ERGO__SECTION__KEY`
([Ergo's manual](https://github.com/ergochat/ergo/blob/stable/docs/MANUAL.md));
the stack sets those GuildNet needs.

## Operator settings

### The install

| Setting | |
|---|---|
| `GUILDNET_DOMAIN` | the domain: the home page (where setup happens) is `https://<domain>`, the network's console `https://<network>.<domain>`. Set by `init.sh`; changing it later means new certificates, and new addresses for everyone |
| `GUILDNET_NETWORK` | the network's name, e.g. `hq`: in its address, and on IRC. 2 to 30 lowercase letters, digits and single hyphens, starting with a letter. Set by `init.sh`; never change it once set up |
| `GUILDNET_VERSION` | the release the install runs, e.g. `26.10.0`: every image is tagged with it. Set by `init.sh` from the repo's `VERSION`; changed only to upgrade ([operations.md](operations.md#upgrading)) |
| `GUILDNET_REGISTRY` | where the images come from (default `ghcr.io/guildnet-chat`): a mirror, or a registry of your own with images built from source |
| `GUILDNET_CHAT_SRC` | where the web chat's code (guildnet-chat) is, to build the console's image from (default `../../../guildnet-chat`, beside this repo) |
| `GUILDNET_SOURCE` | the address of GuildNet's source, which every console page links to (Source). It's under the AGPL: if you change it, publish your changes and link to them here |
| `GUILDNET_CHAT_SOURCE` | the address of the web chat's source, which the chat links to. It's under the AGPL: if you change it, publish your changes and link to them here |

### Email

Optional; see [email.md](email.md).

| Setting | |
|---|---|
| `GUILDNET_MAIL_FROM` | who email is from, e.g. `GuildNet <guildnet@example.com>` |
| `GUILDNET_MAIL_REPLY_TO` | where replies go |
| `GUILDNET_SMTP_ADDR` | the mail server, `host:port` |
| `GUILDNET_SMTP_USERNAME`, `GUILDNET_SMTP_PASSWORD` | if the server wants you to sign in |
| `GUILDNET_SMTP_SECURITY` | `starttls` (the default), `tls` or `none` |
| `GUILDNET_RESEND_KEY` | instead of a mail server: a [Resend](https://resend.com) key that may only send |

### GitHub

Optional; see [github.md](github.md). They're named as the console knows
them: on guildnet.chat this is GuildNet's public app.

| Setting | |
|---|---|
| `GUILDNET_GITHUB_PUBLIC_APP_ID` | your GitHub App's ID |
| `GUILDNET_GITHUB_PUBLIC_APP_SLUG` | its slug, from `github.com/apps/<slug>` |
| `GUILDNET_GITHUB_PUBLIC_CLIENT_ID`, `GUILDNET_GITHUB_PUBLIC_CLIENT_SECRET` | its client ID and a client secret |
| `GUILDNET_GITHUB_PUBLIC_APP_KEY` | its private key, base64-encoded onto one line (`base64 -w0 key.pem`) |

### Git repositories

Optional; see [git.md](git.md). Repositories themselves are added in the
console.

| Setting | |
|---|---|
| `GUILDNET_GIT_PRIVATE_HOSTS` | git servers on your own network rather than the internet that agents' repositories may be on: host names (or addresses), comma-separated. The console tries repositories there, the runner pushes to them, and desks reach them. Unset, every git host must be on the internet |

### Branding

Optional; see [brand.md](brand.md). Files go in `deploy/selfhost/brand/`.

| Setting | |
|---|---|
| `GUILDNET_BRAND_NAME` | what it's called (max 40 characters) |
| `GUILDNET_BRAND_TAGLINE` | what follows the name at the foot of every email |
| `GUILDNET_BRAND_HELLO`, `GUILDNET_BRAND_SUPPORT`, `GUILDNET_BRAND_PRIVACY`, `GUILDNET_BRAND_SECURITY` | the addresses people write to |

### Desks

Optional; see [desks.md](desks.md).

| Setting | |
|---|---|
| `GUILDNET_DESK_MEMORY` | each desk's memory (default `2g`) |
| `GUILDNET_DESK_CPUS` | each desk's CPUs (default `2`) |
| `GUILDNET_DESKS_AT_ONCE` | how many desks work at once (default `1`); the others wait their turn, or the one idle longest is stopped |
| `GUILDNET_DESKS_PER_NETWORK` | on a desks host shared by several networks, how many of those one network may have (default: all of them) |
| `GUILDNET_DESK_FILES` | a folder on the host for desks' files, one folder per network; on XFS with `prjquota`, each network's is limited ([desks.md](desks.md#storage)). Unset: Docker volumes, no limit |
| `GUILDNET_DESK_STORAGE_GB` | how much each network's desks' files may take, in all, with `GUILDNET_DESK_FILES` on XFS (default: no limit) |
| `GUILDNET_DESK_ALLOWED` | where desks may go, comma-separated; `*.example.com` takes its subdomains. Default: GitHub, and the Go, npm and Python registries |
| `GUILDNET_DESK_BLOCKED` | console and runner: hosts no network's desks reach, whatever its owner's list says, beside GuildNet's own blocklist; comma-separated, each blocking the name and every name under it ([desks.md](desks.md#a-networks-own-hosts)) |
| `GUILDNET_DESK_RUNTIME` | the Docker runtime desks run in (default `runsc`, gVisor); `desks.sh --without-gvisor` sets `runc` |
| `GUILDNET_DESK_FIREWALL` | `off`: sandboxd doesn't check, as it starts, that the desks' firewall keeps desks apart (it won't serve if a desk reaches another); `desks.sh --without-firewall` sets it |
| `GUILDNET_MANAGED` | `on`: agents can have desks on Anthropic's Managed Agents instead (they need an Anthropic key, and Anthropic charges for them) |
| `GUILDNET_DESK_BUDGET_CENTS` | the most a Managed Agents desk may spend, in US cents (default 100) |

### Fault reports

| Setting | |
|---|---|
| `GUILDNET_SENTRY_DSN` | where faults in GuildNet's own code are reported: a DSN from a [Sentry](https://sentry.io) or [GlitchTip](https://glitchtip.com) of yours. Unset, they're only in the logs. A report says what went wrong and where, scrubbed of email addresses, keys, tokens, passwords and IP addresses; never anyone's messages |
| `GUILDNET_ENVIRONMENT` | the environment they're reported as (default `production`) |
| `GUILDNET_RELEASE` | the release built into the images as they're built, which reports and the **Server** page show: e.g. `GUILDNET_RELEASE=$(git describe --tags --always) docker compose up -d --build` |
| `GUILDNET_CHAT_COMMIT` | the web chat's commit, built into the console's image, so the chat's Source link points at exactly that commit: e.g. `GUILDNET_CHAT_COMMIT=$(git -C ../../../guildnet-chat rev-parse HEAD)` |

## Generated

Written by `init.sh` (and `desks.sh` for the sandbox keys). The install's data is
encrypted or authenticated with them: never change them; keep `.env` private.

| Setting | |
|---|---|
| `GUILDNET_DB_PASSWORD` | Postgres's own login |
| `GUILDNET_PLATFORM_DB_PASSWORD` | the console's database login |
| `GUILDNET_RUNNER_DB_PASSWORD` | the runner's database login |
| `GUILDNET_PLATFORM_KEY` | 32 random bytes, base64: what the console seals model keys, connectors' tokens and git credentials with, and keys the hashes sign-ins are kept by |
| `GUILDNET_PLATFORM_OLD_KEYS` | while the key's changed: the old ones (comma-separated), which still open and find what they sealed and hashed ([operations.md](operations.md#rotating-the-platform-key)) |
| `GUILDNET_API_TOKEN` | the network's secret: the console calls the runner with it, and the runner the console's broker |
| `GUILDNET_ERGO_API_TOKEN` | how the console and the runner call Ergo's API |
| `GUILDNET_CHAT_KEY` | what web chat's sign-in tokens to Ergo are signed with |
| `GUILDNET_ACCOUNT_SECRET` | what agents' IRC passwords are made from, and what outside agents' webhook secrets (no webhooks without it) and agents' paused conversations, while they wait on an approval, are sealed with |
| `GUILDNET_OPER_PASSWORD` | GuildServ's operator password on Ergo (Ergo hashes it as it starts) |
| `GUILDNET_SANDBOX_KEY` | the console's half of the sandbox's key pair: it signs the runner's desk tokens, and networks' egress passes ([git.md](git.md#reaching-the-host)) |
| `GUILDNET_SANDBOX_PUBLIC_KEY` | sandboxd's and egress's half: they check them (comma-separated, the old and the new, while the pair's changed). A token is good for 15 minutes, and only for its network's desks |
| `GUILDNET_SANDBOX_URL` | where sandboxd is (`https://sandboxd:8443`; on a desks host of its own, `https://<its private address>:8443`) |
| `GUILDNET_DESKS_HOST_ADDRESS` | on a desks host: its private address, the only one sandboxd is published on ([desks-host.md](desks-host.md)) |
| `GUILDNET_SANDBOX_TLS_DIR` | where the console puts sandboxd's certificate (from its own CA), and sandboxd reads it: sandboxd serves TLS when it's set |
| `GUILDNET_SANDBOX_NAMES` | the names and IP addresses sandboxd's certificate is for (default `sandboxd`), comma-separated; a desks host of its own reached by address needs its address here |

`desks.sh` also sets `COMPOSE_PROFILES=desks`, which starts sandboxd and egress
with the stack.

## Set by the stack

Set in `compose.yml`. Needed only when running the programs another way.
Defaults are the programs' own.

### The console (platform)

| Setting | |
|---|---|
| `GUILDNET_BASE_DOMAIN` | the home page's domain; networks' consoles are its subdomains (default `guildnet.localhost`) |
| `GUILDNET_PLATFORM_LISTEN` | where it serves (default `:8080`) |
| `GUILDNET_SECURE_COOKIES` | `on` behind HTTPS: cookies only over HTTPS, and the owner and org-wide admins must use two-step sign-in |
| `GUILDNET_TRUSTED_PROXIES` | the edge's addresses (CIDRs), whose `X-Forwarded-For` is believed |
| `GUILDNET_PLATFORM_DATABASE_URL` | its Postgres database |
| `GUILDNET_NETWORK` | the install's one network, which first-run setup makes |
| `GUILDNET_RUNNER_URL`, `GUILDNET_ERGO_URL`, `GUILDNET_CHAT_URL` | where the network's runner API, Ergo's API and Ergo's WebSocket listener are. Read at every start: moving the install (to a cloud, say) only needs these changed |
| `GUILDNET_CHAT_DIR` | the web chat's built files (the image has them at `/chat`) |
| `GUILDNET_BRAND_DIR` | your own files ([brand.md](brand.md)): the stack mounts `brand/` there |
| `GUILDNET_GITHUB_BROKER_LISTEN` | where the broker serves the runner its settings, keys and tokens, and Ergo people's logins: never on the internet, nor anywhere the edge can reach (the stack: the console's address on the `broker` network only) |
| `GUILDNET_BROKER_TLS_DIR` | where the console keeps the CA it gives the broker its certificate from: a volume only it has |
| `GUILDNET_BROKER_CA_SHARE` | where it puts that CA's certificate for the runner |
| `GUILDNET_BROKER_NAMES` | the names and IP addresses the broker's certificate is for (default `platform`) |
| `GUILDNET_IRC_PORT` | the port IRC apps connect to, as the console tells people (6697) |
| `GUILDNET_IRC_HINT_LISTEN` | where the edge sends IRC apps that forgot TLS, to be told so |
| `GUILDNET_STOP_DRAIN` | once told to stop, how long the console goes on serving while `/healthz` says it's stopping (503), for a load balancer to move off it (default `0s`) |
| `GUILDNET_STOP_GRACE` | then how long requests in flight get to finish, and after them a network it's setting up (default `4s` each: within Docker's 10 seconds) |

### The edge (Caddy)

| Setting | |
|---|---|
| `GUILDNET_DOMAIN`, `GUILDNET_NETWORK` | the names it gets certificates for |
| `GUILDNET_CONSOLE_UPSTREAM` | where it sends the web (default `platform:8080`) |
| `GUILDNET_IRC_UPSTREAM` | where it sends IRC, after TLS, with a PROXY header (default `ergo:6668`) |
| `GUILDNET_IRC_HINT_UPSTREAM` | where it sends IRC apps that forgot TLS (default `platform:6699`) |

### The runner

| Setting | |
|---|---|
| `GUILDNET_NETWORK` | which network it runs |
| `GUILDNET_SETTINGS_FROM_BROKER` | `on`: it waits for first-run setup, and takes the network's settings from the broker |
| `GUILDNET_KEYS_BROKER` | the broker, for its model keys (kept only in memory) |
| `GUILDNET_GITHUB_BROKER` | the broker, for GitHub and connectors' tokens |
| `GUILDNET_BROKER_CA_FILE` | the CA's certificate it trusts the broker by, read at each connection |
| `GUILDNET_TEMPLATES` | the templates a network starts from (default `/app/templates`) |
| `GUILDNET_SERVER` | Ergo, `host:port` (plain IRC, on the private network) |
| `GUILDNET_ERGO_API_URL` | Ergo's API |
| `GUILDNET_DATABASE_URL` | its Postgres database (the org chart, approvals, the log) |
| `GUILDNET_DATA_DIR` | its files: the spend ledger |
| `GUILDNET_OPER_NAME` | GuildServ's operator name on Ergo (`guildserv`) |
| `GUILDNET_API_LISTEN` | where its API serves the console (never the internet) |
| `GUILDNET_WELCOME_DIR` | Ergo's welcome message, shared with Ergo: GuildServ adds what the company is for |

### Ergo

| Setting | |
|---|---|
| `GUILDNET_WEB_URL` | the console's address, in the welcome people see when they connect |
| `GUILDNET_IRC_AUTH_URL` | the console's broker, `/irc/login`: Ergo asks it about people's logins (`guildnet-ircauth`). Unset, Ergo checks its own accounts |
| `GUILDNET_ERGO_TOKEN` | Ergo's API token, which the console knows it by when it asks |
| `GUILDNET_BROKER_CA_FILE` | the console's CA's certificate, the only one Ergo trusts the broker by |

The stack also sets Ergo's own settings: server name, registration off with
SASL required, JWT sign-in for the web chat, the HTTP API, and GuildServ's
operator account.

### Desks: sandboxd and egress

| Setting | |
|---|---|
| `GUILDNET_DESK_IMAGE` | the image desks are made from |
| `GUILDNET_DESK_NETWORK` | the Docker network desks are on (no way out but egress) |
| `GUILDNET_DESK_PROXY` | egress's address, which desks use for HTTPS |
| `GUILDNET_DESK_PIDS` | the most processes a desk may run (default 512) |
| `GUILDNET_SANDBOX_LISTEN` | where sandboxd serves (default `:8443`; never the internet) |
| `GUILDNET_PROXY_LISTEN` | where egress serves desks (default `:3128`) |

## Not for a self-hosted install

Read by the programs, but used by guildnet.chat (a host agent running many
networks) or in development.

| Setting | |
|---|---|
| `GUILDNET_LOCAL_NETWORK`, `GUILDNET_LOCAL_RUNNER_URL`, `GUILDNET_LOCAL_ERGO_URL`, `GUILDNET_LOCAL_CHAT_URL` | development: a network set up by hand rather than by first-run setup |
| `GUILDNET_DEV_SIGNIN` | development: sign in by email alone. Refused with `GUILDNET_SECURE_COOKIES` |
| `GUILDNET_CONFIG` | the runner's settings file; first-run setup picks the template instead |
| `GUILDNET_OWNER`, `GUILDNET_COMPANY`, `GUILDNET_TIMEZONE` | a network's owner, company and timezone; first-run setup gives the runner these |
| `GUILDNET_GITHUB_APP_ID`, `GUILDNET_GITHUB_INSTALLATION_ID`, `GUILDNET_GITHUB_APP_KEY`, `GUILDNET_GITHUB_APP_KEY_FILE` | development: a runner with a GitHub App installation of its own, without the console |
| `GUILDNET_SANDBOX_URL` and `GUILDNET_SANDBOX_KEY` on the runner | development: a runner that signs its own desk tokens, without the console |
| `GUILDNET_BROKER_CA` | guildnet.chat: the host's CA, given to each runner |
| `GUILDNET_PKI_SHARED` | guildnet.chat: where the host shares the broker's certificate |
| `GUILDNET_CERT_CHECK_LISTEN` | guildnet.chat: where the edge asks whether a name is a network's before getting it a certificate |
| `GUILDNET_SANDBOX_WAKE_FILE` | guildnet.chat: how the console wakes the sandbox machine |
| `GUILDNET_REGION` | guildnet.chat: where networks run (default `local`) |
| `GUILDNET_GOOGLE_CLIENT_ID`, `GUILDNET_GOOGLE_CLIENT_SECRET` | guildnet.chat: signing in with Google on the home page |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY` | development: a runner's model keys without the console. A self-hosted install's keys are given in setup and the console's Settings ([models.md](models.md)) |
