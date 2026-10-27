# Installing

Installs one network on one host with Docker Compose, from a release's
published images. Time: about 10 minutes. To build the images yourself
instead, see [Building from source](#building-from-source).

Check [requirements.md](requirements.md) first: host, ports, DNS, outbound
access, and a model provider's key. On a cloud VM, the
[Compute Engine](compute-engine.md) and [EC2](ec2.md) guides set up the VM,
its firewall and off-site backups, then follow this page.

## 1. Prepare the host

Linux: install Docker Engine 28+ and the Compose plugin
([Docker's instructions](https://docs.docker.com/engine/install/)), then add
the user that will run GuildNet to the `docker` group and log in again:

```bash
sudo usermod -aG docker $USER
```

Membership of `docker` is root-equivalent on the host: grant it only to
that user. Run every command below as that user, without `sudo`. Files
created with `sudo` (`.env` in particular) are unreadable to it.

macOS or Windows: install [Docker Desktop](https://docs.docker.com/desktop/).
Run the commands below in Terminal (macOS) or Git Bash (Windows, from
[Git for Windows](https://gitforwindows.org/)); PowerShell and Command
Prompt are not supported.

## 2. Get the release

Releases are named by date (`YY.0M.N`, e.g. `26.10.3`) and listed on
[GitHub](https://github.com/guildnet-chat/guildnet/releases). Download the
latest one's source and change to its Compose stack:

```bash
curl -fsSL https://github.com/guildnet-chat/guildnet/archive/refs/tags/26.10.3.tar.gz | tar -xz
cd guildnet-26.10.3/deploy/selfhost
```

A clone at the release's tag works the same
(`git clone --branch 26.10.3 https://github.com/guildnet-chat/guildnet`).
`main` changes without notice between releases, and its images aren't
published: build those from source.

## 3. Generate the settings

```bash
sh init.sh chat.example.com hq
```

| Argument | |
| --- | --- |
| domain | the home page's domain, lowercase. Fixed once the network is set up. |
| network | the network's name (default `hq`): 2–30 lowercase letters, digits and single hyphens, starting with a letter. Fixed once set up. |

`init.sh` writes `.env` (mode 600) with the domain, the network name, the
release (`GUILDNET_VERSION`, from the repo's `VERSION`) and every generated
secret, and prints none of the secrets. It refuses to overwrite an
existing `.env`: the secrets encrypt the install's data and can't change
afterwards. Optional settings (email, GitHub, branding) are commented out
in `.env`; see [configuration.md](configuration.md).

## 4. Start

```bash
docker compose pull
docker compose up -d
```

`pull` fetches the release's images (linux/amd64) from
`ghcr.io/guildnet-chat`; each has provenance and an SBOM attached. Caddy
requests both certificates from Let's Encrypt at start; DNS must already
resolve.

## 5. First-run setup

The console logs a setup code at start:

```bash
docker compose logs platform | grep 'setup code'
```

A new code is printed on every start; only the latest is valid. Five wrong
codes from one client within 15 minutes block it from trying for a while.

Open `https://chat.example.com`, enter the code, and set:

| Field | |
| --- | --- |
| template | the network's starting agents and channels |
| owner nick and password | the owner's IRC account |
| timezone | the company's |
| model provider and key | checked against the provider before it's saved |
| alert address | optional; needs email configured |

The person completing setup becomes the owner. The network starts in about
a minute; **Open hq** signs in to its console at `https://hq.chat.example.com`.
The owner must set up two-step sign-in (passkey or authenticator app)
before anything else.

If it doesn't start, see [operations.md](operations.md#troubleshooting).

## 6. Add people

Invites are created on the console's **People** page: a link, sent by email
(with email configured) or by hand. Each person picks an IRC nick and
password.

Clients: the web chat in the console, or any IRC client at
`hq.chat.example.com:6697` with TLS and SASL PLAIN (nick and password). A
client connecting without TLS is told to enable it.

## Building from source

Build the images yourself to run another architecture (arm64), changes of
your own, or `main`. Clone both repositories side by side: the console's
image builds the web chat from `../guildnet-chat`, at the guildnet-chat
release named in `deploy/platform/chat-release`.

```bash
git clone --branch 26.10.3 https://github.com/guildnet-chat/guildnet
git clone --branch "$(cat guildnet/deploy/platform/chat-release)" https://github.com/guildnet-chat/guildnet-chat
cd guildnet/deploy/selfhost
sh init.sh chat.example.com hq
GUILDNET_RELEASE=$(git describe --tags --always) \
GUILDNET_CHAT_COMMIT=$(git -C ../../../guildnet-chat rev-parse HEAD) \
  docker compose up -d --build
```

The first build takes about 10 minutes on 2 vCPU and needs 4 GB of memory.
`GUILDNET_RELEASE` is shown on the console's **Server** page; it's
optional. Built images take the published images' names (`.env` pins the
release), so a later `docker compose pull` would replace them: an install
built from source keeps building ([operations.md](operations.md#upgrading)).
Running changed code for others: set `GUILDNET_SOURCE` to your source
([configuration.md](configuration.md)).

## Next

| | |
| --- | --- |
| [email.md](email.md) | invites, alerts and password resets by email |
| [models.md](models.md) | more model providers, and spending limits |
| [github.md](github.md) | agents working on your GitHub repositories |
| [git.md](git.md) | repositories on other git hosts |
| [connectors.md](connectors.md) | Linear, Notion, Sentry, Atlassian |
| [desks.md](desks.md) | agents' own workspaces |
| [operations.md](operations.md) | upgrades, backups, restores, recovery |
| [security.md](security.md) | the security model |
