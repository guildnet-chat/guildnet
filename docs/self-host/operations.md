# Operations

Run all commands in `deploy/selfhost`.

## Status

The console's **Server** page (owner, and anyone with org-wide change
permissions) shows running services, configuration, version and backup
contents, and sends a test email.

```bash
docker compose ps
docker compose logs --since 1h platform
docker compose logs --since 1h runner
```

Services: `caddy`, `platform`, `ergo`, `runner`, `postgres`; with desks,
`sandboxd` and `egress`. Faults in GuildNet's code can also be reported to
Sentry or GlitchTip (`GUILDNET_SENTRY_DSN`, [configuration.md](configuration.md#fault-reports)).

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| The site doesn't load, or the certificate is invalid | Caddy has no certificate; `docker compose logs caddy` says why. Usually DNS (check both names with `dig`) or ports 80/443 closed. Let's Encrypt rate-limits failures: wait an hour after several |
| The build fails, or the host freezes during it | out of memory (4 GB minimum). Add swap (e.g. 4 GB) and rebuild |
| A service restarts repeatedly | `docker compose ps` names it; the end of its log says why. The console exits on invalid settings (a malformed email address, a stray file in `brand/`) and names the setting |
| The runner logs that it's waiting for first-run setup | expected until setup is done ([install.md](install.md#5-first-run-setup)) |
| IRC clients can't connect | they need TLS, port 6697, the network's hostname (`hq.chat.example.com`), and SASL with nick and password |

## Upgrading

An install runs the release `.env` names (`GUILDNET_VERSION`) until it's
changed there. Read the release notes first, then back up.

From published images: get the new release's source beside the old one,
move `.env`, `brand/` and `backups/` across, set the release, and start it.
The data stays where it is, in the Compose project's volumes
(`guildnet_*`), whichever folder the stack is started from.

```bash
sh backup.sh
cd ../../..
curl -fsSL https://github.com/guildnet-chat/guildnet/archive/refs/tags/26.11.0.tar.gz | tar -xz
mv guildnet-26.10.0/deploy/selfhost/.env guildnet-26.10.0/deploy/selfhost/brand \
  guildnet-26.10.0/deploy/selfhost/backups guildnet-26.11.0/deploy/selfhost/
cd guildnet-26.11.0/deploy/selfhost
sed -i.bak 's/^GUILDNET_VERSION=.*/GUILDNET_VERSION=26.11.0/' .env && rm .env.bak
docker compose pull
docker compose up -d
```

Built from source: check out the new release in both repositories, set the
release in `.env` as above, and rebuild:

```bash
sh backup.sh
git fetch --tags && git checkout 26.11.0
git -C ../../../guildnet-chat fetch --tags
git -C ../../../guildnet-chat checkout "$(cat ../platform/chat-release)"
sed -i.bak 's/^GUILDNET_VERSION=.*/GUILDNET_VERSION=26.11.0/' .env && rm .env.bak
GUILDNET_RELEASE=$(git describe --tags --always) \
GUILDNET_CHAT_COMMIT=$(git -C ../../../guildnet-chat rev-parse HEAD) \
  docker compose up -d --build
```

The console and runner migrate their databases at start; releases only
move forward (restore a backup to go back). With desks, also reinstall the
firewall: `sudo sh desks-firewall.sh install`.

## Backup

```bash
sh backup.sh
```

Writes `backups/<UTC time>/`:

| File | Contents |
| --- | --- |
| `env` | `.env`: the install's secrets; nothing else can be read without it |
| `brand.tar` | `brand/` |
| `platform.dump` | the console's database: accounts, network settings, model keys, connector tokens and git credentials (sealed), invites |
| `runner.dump` | the runner's database: org chart, agents, approvals, log |
| `files.tar` | Ergo's data (accounts, channels, history) and the runner's (the spend ledger) |

Ergo and the runner are paused while their files are copied (seconds; no
disconnections). `backup.sh` fails if the copy isn't a complete archive.
Not included: desk workspaces (work leaves as pull requests or pushed
branches) and TLS certificates (reissued).

A backup contains every secret, and a tampered backup could change what's
restored. Store it off the host, encrypted and authenticated, e.g. with
[age](https://age-encryption.org):

```bash
tar -cf - -C backups <time> | age -r <age public key> > guildnet-<time>.tar.age
age -d -i <age key file> guildnet-<time>.tar.age | tar -xf - -C backups
```

`restore.sh` also rejects a backup containing anything a backup doesn't.

Nightly at 03:00, keeping 14 days (`crontab -e`):

```
0 3 * * * cd /path/to/guildnet/deploy/selfhost && sh backup.sh && find backups -mindepth 1 -maxdepth 1 -mtime +14 -exec rm -r {} +
```

## Restore

Into an install with no data (no `.env`, no volumes): Docker set up and a
release's source as in [install.md](install.md), the backup's release or
later. If the backup had desks, install gVisor and the firewall first
([desks.md](desks.md#enabling), steps 1–2).

```bash
sh restore.sh /path/to/backups/<time>
```

It installs the backup's `.env` and files, restores both databases, and
starts the stack at the backup's release (`GUILDNET_VERSION` in its
`.env`), fetching its images if they aren't there. Point DNS at the new
host, then [upgrade](#upgrading) if the source is a later release.

## Account recovery

Owner locked out (lost password or second factor):

```bash
docker compose exec platform /platform reset-owner
```

Prints a single-use link, valid one hour, that sets a new password and
turns the owner's two-step sign-in off until it's set up again. Requires
shell access to the host.

Other people: the owner resets them from their page under **People**.

## Rotating the platform key

`GUILDNET_PLATFORM_KEY` seals the console's secrets (model keys, connector
tokens, git credentials, TOTP secrets) and keys the hashes of sessions, links and recovery
codes. Rotate it if it may have leaked:

1. Generate: `head -c 32 /dev/urandom | base64`.
2. In `.env`: the new key in `GUILDNET_PLATFORM_KEY`, the old one in
   `GUILDNET_PLATFORM_OLD_KEYS`.
3. `docker compose up -d`.

Values sealed with the old key still open and are resealed with the new one
when next saved. Keep the old key listed: sessions, links and recovery
codes are looked up by it until replaced (recovery codes only when two-step
is set up again).

## Uninstalling

Deletes the network and all its data. Back up first if needed.

With desks, remove them first (sandboxd creates them outside Compose, as
`gn.<network>.<nick>`), then the firewall. Replace `hq` with the network's
name:

```bash
docker ps -aq --filter 'name=^gn\.hq\.' | xargs -r docker rm -f
docker volume ls -q --filter 'name=gn.hq.' | xargs -r docker volume rm
sudo systemctl disable --now guildnet-desks-firewall.service
sudo rm -r /etc/systemd/system/guildnet-desks-firewall.service /usr/local/lib/guildnet /etc/modules-load.d/guildnet-desks.conf /etc/sysctl.d/90-guildnet-desks.conf
```

The firewall rules are cleared at the next reboot. Then remove the stack,
its volumes and images:

```bash
docker compose --profile desks --profile tools down -v --rmi all
```

`.env`, `brand/` and `backups/` remain; delete them when certain.
