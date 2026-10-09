# deploy/desks-host

Desks on a VM of their own, for a console that runs elsewhere (GKE,
Fargate, or another machine). Guide:
[docs/self-host/desks-host.md](../../docs/self-host/desks-host.md).

| Service | |
| --- | --- |
| `sandboxd` | agents' workspaces, under gVisor; published on the host's private address only (`GUILDNET_DESKS_HOST_ADDRESS`), port 8443, TLS |
| `egress` | the desks' only way out: HTTPS to allowed hosts, and to a network's git hosts with its egress pass |
| `desk` | the desk image, fetched or built, never run here (`scale: 0`) |
| `tls` | profile `tools`: sandboxd's key, its certificate request, and installing the certificate the console signs |

| Script | |
| --- | --- |
| `init.sh` | generates `.env`, the console's settings (`console.env`) and the certificate request (`sandboxd.csr`) |

The desks' firewall is `../selfhost/desks-firewall.sh`, as for desks beside
the console.
