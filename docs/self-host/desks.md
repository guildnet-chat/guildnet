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
| The `desks` network | `internal` Docker network, 172.31.255.0/24, with no route out |
| `egress` | the only way out: an HTTPS (CONNECT) proxy to port 443 of the hosts in `GUILDNET_DESK_ALLOWED`, and to a network's git hosts with its egress pass ([git.md](git.md#reaching-the-host)); public addresses only. It isn't on the console's, runner's or database's networks |
| `desks-firewall.sh` | iptables rules: a desk reaches `egress` and nothing else on its network, not other desks and not the host |
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
- Keep `GUILDNET_DESK_ALLOWED` short.
- Desk output counts as untrusted: a write through a connector shortly
  after (a Linear issue, say) asks a person first, whatever its policy.

## Enabling

1. Install gVisor ([guide](https://gvisor.dev/docs/user_guide/install/))
   and register it with Docker:

   ```bash
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

The console's **Server** page shows desks as on.

### Without gVisor or the firewall

| Flag | Effect | Use |
| --- | --- | --- |
| `--without-gvisor` | desks use Docker's default runtime (`runc`) and share the host kernel | trials only |
| `--without-firewall` | desks can reach each other and anything the host listens on | trials only, e.g. Docker Desktop (no systemd) |

A desk created before gVisor was enabled, or before its limits or image
changed, is recreated with the new settings on its next start. Its files
persist.

## Sizing

| Setting | Default | |
| --- | --- | --- |
| `GUILDNET_DESKS_AT_ONCE` | 1 | desks running at once. When full, the desk idle longest is stopped, or the request waits |
| `GUILDNET_DESK_MEMORY` | `2g` | memory per desk |
| `GUILDNET_DESK_CPUS` | 2 | CPUs per desk |

Leave 2–3 GB for the rest of the stack. Example, 16 GB host: 4 desks at once,
3 GB each. Idle desks are stopped after 10 minutes; their files persist.
Apply changes with `docker compose up -d`.

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

## Managed Agents

With an Anthropic key, agents can use desks on Anthropic's
[Managed Agents](https://docs.anthropic.com) instead, run and billed by
Anthropic. Set `GUILDNET_MANAGED=on`, and `GUILDNET_DESK_BUDGET_CENTS` for
each desk's lifetime spend limit (default 100, i.e. $1). Claude models only.

## Backups

Desk workspaces aren't backed up. Agents' work leaves desks as pull
requests, or as branches pushed to other git hosts.
