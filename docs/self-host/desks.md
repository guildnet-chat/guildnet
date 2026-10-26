# Desks

Optional. A desk is an agent's own workspace: a container from the desk
image (Go, Node, Python, git, ripgrep) where the agent clones its team's
repositories, builds and runs tests. Without desks, agents work in chat and
reach repositories through GitHub's API; repositories on other git hosts
([git.md](git.md)) need a desk.

Desks need Linux: gVisor and the desks' firewall don't run on Docker
Desktop ([requirements.md](requirements.md)). This page runs them beside the
console; [desks-host.md](desks-host.md) runs them on a VM of their own (for a
console on GKE or Fargate, say).

## Isolation

| Layer | |
| --- | --- |
| gVisor (`runsc`) | each desk gets its own kernel; a container escape reaches gVisor, not the host kernel |
| The `desks` network | `internal` Docker network, 172.31.255.0/24, with no route out; IPv4 only (`desks-firewall.sh` refuses a desks' network with IPv6) |
| `egress` | the only way out: an HTTPS (CONNECT) proxy to port 443 of the hosts in `GUILDNET_DESK_ALLOWED`, and to a network's git hosts and its owner's hosts with its egress pass ([git.md](git.md#reaching-the-host), [below](#a-networks-own-hosts)); public addresses only. It isn't on the console's, runner's or database's networks |
| `desks-firewall.sh` | iptables rules: a desk reaches `egress` and nothing else on its network, not other desks and not the host. sandboxd checks it as it starts (one probe desk tries to reach another) and won't serve if it isn't working |
| sandboxd | creates and stops desks. It holds the Docker socket (root-equivalent), so it's on the private network only and acts only on a console-signed token: valid 15 minutes, for one network's desks; only the console's admin token removes all of them |
| Limits | memory, CPUs and processes per desk; commands run as an unprivileged user |
| Fresh start | each time a desk is readied, shell, git, Go, npm and pip startup files in its home are reset to the image's, and commands run without reading shell startup files. Caches and the workspace persist |

Details: [architecture.md](../architecture.md#7-desks).

## Residual risk

Anything a desk can read can be sent to an allowed host, and allowed hosts
accept uploads. An agent manipulated by content it read (an issue, a pull
request, a web page) could push code to another GitHub repository or
publish a package with credentials supplied by an attacker. The proxy
blocks everything else, not that.

Mitigations:

- Give teams only the repositories they need; a desk clones only its team's.
- Keep `GUILDNET_DESK_ALLOWED`, and each network's own hosts, short.
- Desk output counts as untrusted: a write through a connector shortly
  after (a Linear issue, say) asks a person first, whatever its policy.

## Enabling

1. Install gVisor and register it with Docker. On Ubuntu or Debian, from
   gVisor's repository (other systems: [gVisor's guide](https://gvisor.dev/docs/user_guide/install/)):

   ```bash
   curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor --yes -o /usr/share/keyrings/gvisor-archive-keyring.gpg
   echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
     | sudo tee /etc/apt/sources.list.d/gvisor.list >/dev/null
   sudo apt-get update && sudo apt-get install -y runsc
   sudo runsc install
   sudo systemctl restart docker
   ```

2. In `deploy/selfhost`, install the desks' firewall. A systemd unit
   reapplies it whenever Docker starts:

   ```bash
   sudo sh desks-firewall.sh install
   ```

3. Enable desks. This generates the desks' token key pair into `.env`
   (not printed) and turns on the `desks` profile. It refuses until gVisor
   and the firewall are present:

   ```bash
   sh desks.sh
   ```

4. Fetch the desk image and start sandboxd and egress:

   ```bash
   docker compose pull
   docker compose up -d
   ```

   Built from source: `docker compose up -d --build` instead (the desk
   image takes several minutes).

The console's **Server** page shows desks as on once the runner has taken
them up, within a minute; agents can then be moved to desks on their pages.

### Without gVisor or the firewall

| Flag | Effect | Use |
| --- | --- | --- |
| `--without-gvisor` | desks use Docker's default runtime (`runc`) and share the host kernel | trials only |
| `--without-firewall` | desks can reach each other and anything the host listens on; sandboxd doesn't check for the firewall (`GUILDNET_DESK_FIREWALL=off`) | trials only, e.g. Docker Desktop (no systemd) |

A desk created before gVisor was enabled, or before its limits or image
changed, is recreated with the new settings on its next start. Its files
persist.

## Sizing

| Setting | Default | |
| --- | --- | --- |
| `GUILDNET_DESKS_AT_ONCE` | 1 | desks running at once. When full, the desk idle longest is stopped, or the request waits |
| `GUILDNET_DESKS_PER_NETWORK` | unset | for a desks host shared by several networks: how many of those desks one network may have. At its share, a network's new desk stops one of its own idle ones, or waits |
| `GUILDNET_DESK_MEMORY` | `2g` | memory per desk |
| `GUILDNET_DESK_CPUS` | 2 | CPUs per desk |

Leave 2–3 GB for the rest of the stack. Example, 16 GB host: 4 desks at once,
3 GB each. Idle desks are stopped after 10 minutes; their files persist.
Apply changes with `docker compose up -d`.

## Storage

By default each desk's files are on two Docker volumes, with no limit: one
desk can fill the host's disk.

To limit them, give desks' files a folder of their own on an XFS filesystem
mounted with `prjquota`, and set these in `.env`, then apply them with
`docker compose up -d`:

| Setting | |
| --- | --- |
| `GUILDNET_DESK_FILES` | the folder, on the host. Each network's desks' files go in a folder of the network's there |
| `GUILDNET_DESK_STORAGE_GB` | how much each network's desks' files may take, in all. A console with a desk policy (plans) sets it per network instead |

sandboxd sets each limit as an XFS project quota, from a short-lived
privileged container of the desk image. A desk at its network's limit gets
"No space left on device", and its agent is told the network's desks'
storage is full; other networks' desks aren't affected. sandboxd reports
when the folder's disk is 80% full (the limits can add up to more than the
disk), and when the disk Docker keeps images and containers on is.

The owner sees how much of the limit is used in Settings, and each agent's
page says what its desk holds. The owner is emailed once as use passes 80%,
and once as it fills. Whoever may change an agent can clear its desk from
its page: its work folder and home are deleted, and its repos are checked
out again at its next wake-up. The console asks sandboxd only while it's
up (asking never wakes the sandbox), and shows what it last said meanwhile.

An XFS filesystem in a file on an existing disk (here 30 GB, allocated in
full so it can't fill the host's disk; `-K` keeps `mkfs.xfs` from
discarding the file, which would leave it sparse):

```bash
sudo apt-get install -y xfsprogs
sudo fallocate -l 30G /var/lib/guildnet-desks.img
sudo mkfs.xfs -K /var/lib/guildnet-desks.img
sudo mkdir -p /var/lib/guildnet/desks
echo '/var/lib/guildnet-desks.img /var/lib/guildnet/desks xfs loop,prjquota 0 0' | sudo tee -a /etc/fstab
sudo mount /var/lib/guildnet/desks
```

Desks made before `GUILDNET_DESK_FILES` was set have their files moved into
it the next time they're made, and their volumes are removed.

## Allowed hosts

Default `GUILDNET_DESK_ALLOWED`: `github.com`, `api.github.com`,
`codeload.github.com`, `*.githubusercontent.com`, `proxy.golang.org`,
`sum.golang.org`, `dl.google.com`, `registry.npmjs.org`, `pypi.org`,
`files.pythonhosted.org`.

Setting it replaces the list: comma-separated hosts, `*.example.com` for
subdomains. Keep GitHub's hosts. Example, adding Rust's registry:

```sh
GUILDNET_DESK_ALLOWED=github.com,api.github.com,codeload.github.com,*.githubusercontent.com,proxy.golang.org,sum.golang.org,dl.google.com,registry.npmjs.org,pypi.org,files.pythonhosted.org,crates.io,static.crates.io
```

### A network's own hosts

Past that list, each network's owner chooses more hosts for its desks, on
the console's Connectors page ("Hosts desks may reach"). Anyone else asks:
a person there, or an agent whose desk was refused a host (its
`request_host` tool). It goes to the owner, and only the owner, as an
approval, and approving adds the host. Each change is in the org log and
emailed to the owner.

- Exact host names only (no `*.`), on port 443. Never an address, a
  single-label name, or one ending `.internal`, `.local`, `.localhost`,
  `.lan`, `.home.arpa` and the like.
- A blocklist wins whatever an owner's list says: GuildNet's own (paste
  sites, file drops, request catchers and tunnels, in
  `internal/sandbox/hosts.go`) and `GUILDNET_DESK_BLOCKED` (comma-separated,
  on the console and runner; each blocks the name and every name under it).
- The hosts reach `egress` in the network's egress pass, which the console
  signs, as for its git hosts: `egress` needs `GUILDNET_SANDBOX_PUBLIC_KEY`.
  Each command at a desk carries the pass as it is then, so a host approved
  while an agent works opens from its next command.

## Managed Agents

With an Anthropic key, agents can use desks on Anthropic's
[Managed Agents](https://docs.anthropic.com) instead, run and billed by
Anthropic. Set `GUILDNET_MANAGED=on`, and `GUILDNET_DESK_BUDGET_CENTS` for
each desk's lifetime spend limit (default 100, i.e. $1). Claude models only.

## Backups

Desk workspaces aren't backed up. Agents' work leaves desks as pull
requests, or as branches pushed to other git hosts.
