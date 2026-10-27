# Desks on a host of their own

Desks need a Linux host's Docker with gVisor, which GKE and Fargate don't
give a container. With the console there, or to keep desks off the
console's machine, a separate VM runs sandboxd, egress and the desk image
(`deploy/desks-host`). The console and the runner reach sandboxd over the
private network, at the VM's private address, over TLS with a certificate
the console signs. Desks themselves work as in [desks.md](desks.md).

| | |
| --- | --- |
| Host | Linux VM, Docker Engine 28+, gVisor, the desks' firewall; sized as in [requirements.md](requirements.md) "With desks" (stack's share excepted) |
| Network | the same private network as the console and runner (a VPC); a private IPv4 address |
| Inbound | tcp 8443 from the console and the runner only (their VM, GKE's pod range, or Fargate's security group). Nothing from the internet |
| Outbound | what `GUILDNET_DESK_ALLOWED` lets desks reach, networks' own hosts, and npm's registry (read-only), through egress; `ghcr.io` for images. Not git hosts: the runner fetches desks' repos |

## 1. The VM

As in the [Compute Engine](compute-engine.md) or [EC2](ec2.md) guide, with
no public ports and, on Compute Engine, no external address
(`--no-address`; images come through Cloud NAT). Let only the console in,
e.g. on Compute Engine, from a VM tagged `guildnet`:

```bash
gcloud compute firewall-rules create guildnet-desks-host --network default \
  --allow tcp:8443 --source-tags guildnet --target-tags guildnet-desks
```

From GKE, use `--source-ranges` with the cluster's pod range instead. The
`default` network's `default-allow-internal` rule admits every port between
its VMs, so it would let any of them reach sandboxd: on a network shared
with other VMs, delete or narrow it, or use a network of your own.

Install Docker, then gVisor and the desks' firewall
([desks.md](desks.md#enabling), steps 1–2; the firewall script is
`deploy/selfhost/desks-firewall.sh`).

## 2. Settings

Get the release the console runs ([install.md](install.md#2-get-the-release)),
then, with the VM's private address:

```bash
cd guildnet-26.10.2/deploy/desks-host
sh init.sh 10.0.0.5
```

| File | |
| --- | --- |
| `.env` | this host's settings: the release, its address, sandboxd's half of the key pair. Mode 600 |
| `console.env` | for the console: its half of the key pair (`GUILDNET_SANDBOX_KEY`), `GUILDNET_SANDBOX_URL=https://10.0.0.5:8443`, `GUILDNET_SANDBOX_NAMES=10.0.0.5`. Mode 600 |
| `sandboxd.csr` | a request for sandboxd's certificate. Its key stays in the host's `tls` volume |

`init.sh` refuses a public address, an existing `.env`, and a host without
gVisor or the firewall (`--without-gvisor`, `--without-firewall` override,
as for [desks.sh](desks.md)).

## 3. The console

Add `console.env`'s three settings to the console's: on a Compose install,
append them to its `.env` (and don't turn desks on there); on GKE, to its
Secret. Restart the console (`docker compose up -d`), then delete
`console.env` from the desks host.

Sign the request with the console's CA, for the names in
`GUILDNET_SANDBOX_NAMES`. On a Compose install:

```bash
docker compose exec -T platform /platform sandbox-cert < sandboxd.csr > sandboxd.crt
```

On GKE: `kubectl exec -i guildnet-0 -c platform -- /platform sandbox-cert < sandboxd.csr > sandboxd.crt`.
The certificate is public; copy it to the desks host.

## 4. Start

On the desks host:

```bash
docker compose run --rm -T tls tls-install < sandboxd.crt
docker compose pull
docker compose up -d
```

`tls-install` refuses a certificate that isn't for the host's key. The
console's **Server** page shows **Desks (sandboxd)** as running, with its
certificate's end date.

## Renewing the certificate

It lasts a year, and isn't renewed on its own. From three weeks before it
runs out, the Server page and the console's log say so. Make a new request
(the key stays the same), sign it as in step 3, and install it; sandboxd
picks it up without restarting:

```bash
docker compose run --rm -T tls tls-request > sandboxd.csr
docker compose run --rm -T tls tls-install < sandboxd.crt
```

## Upgrading

Run the console's release. With the console upgraded
([operations.md](operations.md#upgrading)), get the same release here, move
`.env` across, set `GUILDNET_VERSION` in it, and `docker compose pull && docker compose up -d`.
Reinstall the firewall: `sudo sh ../selfhost/desks-firewall.sh install`.

## Security

sandboxd drives the host's Docker, so it's root-equivalent on the desks
host. It's reachable only on the private address the cloud's firewall
limits, serves TLS 1.3 with a certificate the console's CA signed for that
address, and does nothing without a token the console signed for a
network. The console and runner check its certificate against the address
they dial. Its key never leaves the host. What desks can and can't reach is
as in [security.md](security.md).
