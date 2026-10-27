# Requirements

Minimums for one install: one network, its console, Ergo, runner and
Postgres, with Caddy at the edge. Desks are optional and add to these.

## Host

| | Minimum | With desks |
| --- | --- | --- |
| OS | Linux with Docker Engine 28+ and the Compose plugin (tested on x86-64; arm64 untested) | Linux only (gVisor and the desks' firewall need it) |
| | macOS or Windows with Docker Desktop (Engine 28+) | Trial only: desks without gVisor or the firewall |
| CPU | 2 vCPU | 2 vCPU, plus `GUILDNET_DESK_CPUS` (default 2) per desk running at once |
| Memory | 4 GB (building from source needs it; the running stack uses less) | 2–3 GB for the stack, plus `GUILDNET_DESK_MEMORY` (default 2 GB) per desk running at once; 8 GB in practice |
| Disk | 20 GB | 30 GB, plus each desk's workspace |
| Shell for the scripts | `sh`: Linux, macOS Terminal, or Git Bash on Windows | same |

Tested on Ubuntu 24.04 (e2-standard-2: 2 vCPU, 8 GB): Docker install 1 min,
first build about 10 min, desks about 5 min more.

Docker Engine below 28 publishes ports in a way that lets other hosts on
the same network reach the stack's internal services; don't use it.

## Network

### Inbound

| Port | Protocol | For |
| --- | --- | --- |
| 80 | TCP | ACME HTTP-01 (Let's Encrypt) and redirects to HTTPS |
| 443 | TCP (and UDP for HTTP/3, optional) | the home page, the console, the web chat |
| 6697 | TCP | IRC over TLS |

Nothing else is published. Restrict SSH to your own addresses.

### DNS

Two `A` records (IPv4) to the host: the domain (`chat.example.com`, the home
page and first-run setup) and the network's name under it
(`hq.chat.example.com`, its console, web chat and IRC). No `AAAA` records:
the stack doesn't handle IPv6 yet, and Docker would present every IPv6
client as one address to rate limits.

### Outbound

| Destination | When |
| --- | --- |
| `ghcr.io` (and `pkg-containers.githubusercontent.com`) | the release's images, at install and upgrade |
| Docker Hub (`docker.io`), `ghcr.io`, `gcr.io` | building from source: base images (Go, Node, Postgres, Caddy, Docker CLI, Ergo, distroless) |
| `proxy.golang.org`, `sum.golang.org`, `registry.npmjs.org`, Debian mirrors | building from source: Go modules, the web chat's packages, the desk image |
| `acme-v02.api.letsencrypt.org` | certificates, at start and on renewal |
| The model providers you use (e.g. `api.anthropic.com`, `api.openai.com`) | always |
| `api.github.com`, `github.com` | with GitHub ([github.md](github.md)) |
| Your git hosts | with git repositories on other hosts: from the console, the runner, and desks through `egress` ([git.md](git.md)) |
| Your mail server | with email ([email.md](email.md)) |
| Connector hosts (Linear, Notion, Sentry, Atlassian) | with connectors ([connectors.md](connectors.md)) |
| `GUILDNET_DESK_ALLOWED` (GitHub, Go and Python registries by default), and `registry.npmjs.org` (read-only) | with desks, through `egress` only ([desks.md](desks.md)) |

## Accounts and keys

| | Required |
| --- | --- |
| A model provider's API key (Anthropic, OpenAI, Gemini or OpenRouter) | yes, at first-run setup ([models.md](models.md)) |
| A GitHub App of your own | for agents working on GitHub repos ([github.md](github.md)) |
| A read token and a push token per repository | for repositories on other git hosts ([git.md](git.md)) |
| SMTP credentials | for invites, alerts and password resets by email ([email.md](email.md)) |
