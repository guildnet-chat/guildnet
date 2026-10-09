# deploy/gke

The stack in one pod on GKE Autopilot, with Postgres on Cloud SQL. Guide:
[docs/self-host/gke.md](../../docs/self-host/gke.md).

| File | |
| --- | --- |
| `kustomization.yaml` | applied with `kubectl apply -k .`; names the release's images |
| `statefulset.yaml` | one pod: Caddy, the console, Ergo, the runner, with the stack's settings; `hostAliases` give their names to localhost |
| `service.yaml` | the edge's load balancer (80, 443, 6697), and a network policy admitting only those |
| `db-init.yaml` | a Job making the console's and runner's databases on Cloud SQL |
| `restore.yaml` | a pod for restoring a `backup.sh` backup (applied on its own) |
| `address.yaml.example` | the edge's reserved address, copied to `address.yaml` |

An install's own files, never committed (`.gitignore`): `guildnet.env`
(its settings and secrets), `server-ca.pem` (Cloud SQL's CA),
`address.yaml`.