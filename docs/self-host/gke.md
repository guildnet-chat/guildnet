# Google Cloud: GKE

The stack in one pod on GKE Autopilot (`deploy/gke`), with Postgres on
Cloud SQL and desks on a VM of their own ([desks-host.md](desks-host.md)).
The pod runs the release's images with the stack's settings; `hostAliases`
give `platform`, `ergo` and `runner` to localhost, so addresses and
certificate names are as in Compose, and a Compose backup restores here
unchanged.

| Resource | Setting |
| --- | --- |
| Cluster | GKE Autopilot, regional; network policies enforced (Dataplane V2) |
| Pod | a StatefulSet of one: Caddy, the console, Ergo, the runner. Updates replace the pod; two never run |
| Disk | one 10 GB persistent disk: Ergo's data, the runner's ledger, Caddy's certificates, the console's CA |
| Database | Cloud SQL for PostgreSQL, private IP only, TLS required (checked against its server CA), automated backups with point-in-time recovery |
| Edge | a passthrough load balancer on a reserved address, visitors' addresses kept (`externalTrafficPolicy: Local`); Caddy terminates TLS as in Compose |
| Isolation | a network policy admitting only 80, 443 and 6697; namespace at the `baseline` pod security level |
| Desks | a Compute Engine VM ([desks-host.md](desks-host.md)) |

About $85–100/month in `us-central1` (pod requests, Cloud SQL
`db-g1-small`, disk, load balancer), plus the desks VM.

Tested on GKE Autopilot 1.35 with Cloud SQL for PostgreSQL 18: a Compose
install's backup restored, Let's Encrypt certificates for both names, IRC
over TLS, visitors' own addresses at the edge, desks on a VM reached from
the pod, and updates replacing the pod without a second.

Differences from Compose: inside one pod, Caddy can reach the console's
broker and the runner's API (both still need their secrets); Ergo and the
console believe forwarded addresses from localhost only.

## 1. Project and network

From a machine with the [gcloud CLI](https://cloud.google.com/sdk/docs/install),
`kubectl` and GKE's credential plugin
(`gcloud components install gke-gcloud-auth-plugin`), as a project owner:

```bash
PROJECT=my-project
REGION=us-central1
gcloud config set project $PROJECT
gcloud services enable container.googleapis.com sqladmin.googleapis.com servicenetworking.googleapis.com
```

Private services access, for Cloud SQL's private address on the `default`
network:

```bash
gcloud compute addresses create google-managed-services-default --global \
  --purpose=VPC_PEERING --prefix-length=20 --network=default
gcloud services vpc-peerings connect --service=servicenetworking.googleapis.com \
  --ranges=google-managed-services-default --network=default
```

## 2. Settings

Get the release ([install.md](install.md#2-get-the-release)) and make the
settings with the Compose stack's `init.sh`, then move them into
`deploy/gke`:

```bash
cd guildnet-26.10.1/deploy/selfhost
sh init.sh chat.example.com hq
cd ../gke
grep -v '^#' ../selfhost/.env | grep . > guildnet.env && chmod 600 guildnet.env
```

Keep a copy of `guildnet.env` somewhere safe (a secret manager): its
secrets encrypt the install's data. Optional settings (email, GitHub,
branding) go in it as in [configuration.md](configuration.md).

## 3. Cloud SQL

```bash
gcloud sql instances create guildnet --region $REGION \
  --database-version POSTGRES_18 --edition ENTERPRISE --tier db-g1-small \
  --network default --no-assign-ip --ssl-mode ENCRYPTED_ONLY \
  --storage-size 10 --storage-auto-increase \
  --backup-start-time 03:00 --enable-point-in-time-recovery
gcloud sql users set-password postgres --instance guildnet \
  --password "$(grep '^GUILDNET_DB_PASSWORD=' guildnet.env | cut -d= -f2)"
echo "GUILDNET_DB_HOST=$(gcloud sql instances describe guildnet --format 'value(ipAddresses[0].ipAddress)')" >> guildnet.env
gcloud sql instances describe guildnet --format 'value(serverCaCert.cert)' > server-ca.pem
```

## 4. Cluster and address

```bash
gcloud container clusters create-auto guildnet --region $REGION --network default
gcloud container clusters get-credentials guildnet --region $REGION
gcloud compute addresses create guildnet --region $REGION
sed "s/203.0.113.10/$(gcloud compute addresses describe guildnet --region $REGION --format 'value(address)')/" \
  address.yaml.example > address.yaml
```

Point both DNS `A` records at the address ([requirements.md](requirements.md#dns)).

## 5. Start

```bash
kubectl apply -k .
kubectl -n guildnet wait --for=condition=complete job/guildnet-db-init --timeout=5m
kubectl -n guildnet rollout status statefulset/guildnet --timeout=10m
kubectl -n guildnet logs guildnet-0 -c platform | grep 'setup code'
```

`guildnet-db-init` makes the console's and runner's databases and logins
on Cloud SQL, as the `postgres` admin (safe to run again). The console
restarts until they exist. Then open `https://chat.example.com` and set the
network up ([install.md](install.md#5-first-run-setup)).

Caddy asks Let's Encrypt for certificates as soon as it starts, and the
load balancer sends nothing to the pod until all of it is ready: while
another container restarts, those attempts fail, and after five failures
Let's Encrypt refuses the name for an hour. `kubectl -n guildnet logs
guildnet-0 -c caddy` says so; Caddy tries again on its own.

## Operating

| Task | Command |
| --- | --- |
| Logs | `kubectl -n guildnet logs guildnet-0 -c platform` (or `caddy`, `ergo`, `runner`) |
| Locked-out owner | `kubectl -n guildnet exec guildnet-0 -c platform -- /platform reset-owner` |
| Changed settings | edit `guildnet.env`, `kubectl apply -k .`, `kubectl -n guildnet rollout restart statefulset/guildnet` |
| Branding | `kubectl -n guildnet create configmap guildnet-brand --from-file=brand/`, then restart |
| Desks | [desks-host.md](desks-host.md); the firewall's source is the cluster's pod range (`gcloud container clusters describe guildnet --region $REGION --format 'value(clusterIpv4Cidr)'`) |

### Upgrading

Read the release notes. Get the new release, copy `guildnet.env`,
`server-ca.pem` and `address.yaml` into its `deploy/gke` (its
`kustomization.yaml` names its images), and apply from there:

```bash
cp ../../../guildnet-26.10.0/deploy/gke/{guildnet.env,server-ca.pem,address.yaml} .
kubectl apply -k .
```

The pod is replaced, never doubled; the console and runner migrate their
databases at start.

### Backups

Cloud SQL keeps automated backups and point-in-time recovery. For the disk,
a snapshot schedule on its persistent disk
(`gcloud compute resource-policies create snapshot-schedule`, as in the
[Compute Engine guide](compute-engine.md#2-the-vm), attached to the disk
`kubectl -n guildnet get pv` names). A backup in Compose's format (to move
between Compose and GKE) is `backup.sh`'s files; restoring one is below.

## Restoring a backup

From `backup.sh` (Compose's, or made the same way here), into an install
with no data yet: steps 1–4 with the backup's `env` as `guildnet.env` (plus
`GUILDNET_DB_HOST`), then:

```bash
kubectl apply -k .
kubectl -n guildnet wait --for=condition=complete job/guildnet-db-init --timeout=5m
kubectl -n guildnet scale statefulset/guildnet --replicas=0
kubectl -n guildnet apply -f restore.yaml
kubectl -n guildnet wait --for=condition=ready pod/guildnet-restore
for f in platform.dump runner.dump files.tar; do
  kubectl -n guildnet cp backups/<time>/$f guildnet-restore:/backup/$f
done
kubectl -n guildnet exec guildnet-restore -- sh -c '
  PGUSER=platform PGPASSWORD=$PLATFORM_DB_PASSWORD pg_restore --dbname=platform --no-owner --clean --if-exists --exit-on-error /backup/platform.dump &&
  PGUSER=runner PGPASSWORD=$RUNNER_DB_PASSWORD pg_restore --dbname=runner --no-owner --clean --if-exists --exit-on-error /backup/runner.dump &&
  tar -xf /backup/files.tar -C /disk'
kubectl -n guildnet delete pod guildnet-restore
kubectl -n guildnet scale statefulset/guildnet --replicas=1
```

The console may have started (and made empty tables) before the scale-down:
`--clean` replaces them. `files.tar` holds `ergo/` and `runner/`, the same
folders as on the pod's disk. Brand files go in the `guildnet-brand`
ConfigMap. Point DNS at the new address.
