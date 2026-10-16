# Google Cloud: Compute Engine

The Compose stack ([install.md](install.md)) on one Compute Engine VM, with
a static address, a firewall that admits only the stack's ports, SSH
through IAP, daily disk snapshots, and nightly encrypted backups in a
Cloud Storage bucket. Tested on Ubuntu 24.04 LTS, e2-standard-2.

| Resource | Setting |
| --- | --- |
| VM | `e2-standard-2` (2 vCPU, 8 GB); `e2-standard-4` with desks. Ubuntu 24.04 LTS, x86-64, Shielded VM |
| Boot disk | 30 GB `pd-balanced`; 50 GB with desks |
| Address | reserved static external IPv4 |
| Firewall | tcp 80, 443, 6697 and udp 443 from anywhere; tcp 22 from IAP (`35.235.240.0/20`) only |
| Service account | its own, with two roles, on the backup bucket only: create and read objects |
| Backups | daily disk snapshots (14 days); nightly `backup.sh`, encrypted with [age](https://age-encryption.org), to a bucket with a 30-day retention policy |

About $55/month in `us-central1` on demand (VM, disk, address), before
network egress and snapshot storage.

## 1. Project resources

Run these from a machine with the [gcloud CLI](https://cloud.google.com/sdk/docs/install),
signed in as a project owner. Set the variables first:

```bash
PROJECT=my-project
REGION=us-central1
ZONE=us-central1-a
BUCKET=$PROJECT-guildnet-backups
gcloud config set project $PROJECT
gcloud services enable compute.googleapis.com storage.googleapis.com iap.googleapis.com
```

Address, service account and bucket:

```bash
gcloud compute addresses create guildnet --region $REGION
gcloud compute addresses describe guildnet --region $REGION --format 'value(address)'

gcloud iam service-accounts create guildnet-vm --display-name "GuildNet VM"
SA=guildnet-vm@$PROJECT.iam.gserviceaccount.com

gcloud storage buckets create gs://$BUCKET --location $REGION \
  --uniform-bucket-level-access --public-access-prevention --retention-period 30d
for role in roles/storage.objectCreator roles/storage.objectViewer; do
  gcloud storage buckets add-iam-policy-binding gs://$BUCKET \
    --member serviceAccount:$SA --role $role
done
```

The service account can add backups and read them (encrypted to a key
that isn't on the VM; `gcloud storage cp` needs to read), but not replace
or delete them, and the retention policy stops anyone deleting one for 30
days: a compromised VM can't destroy the backups it made. Add a lifecycle
rule to delete objects after 90 days if wanted.

Firewall rules, for VMs tagged `guildnet`:

```bash
gcloud compute firewall-rules create guildnet-web --network default \
  --allow tcp:80,tcp:443,udp:443,tcp:6697 --target-tags guildnet
gcloud compute firewall-rules create guildnet-ssh-iap --network default \
  --allow tcp:22 --source-ranges 35.235.240.0/20 --target-tags guildnet
```

The `default` network's `default-allow-ssh` rule admits SSH from anywhere,
to every VM: delete it, or restrict its source ranges, unless something
else needs it.

## 2. The VM

```bash
gcloud compute instances create guildnet --zone $ZONE \
  --machine-type e2-standard-2 \
  --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud \
  --boot-disk-size 30GB --boot-disk-type pd-balanced \
  --address guildnet --tags guildnet \
  --service-account $SA --scopes cloud-platform \
  --shielded-secure-boot --shielded-vtpm --shielded-integrity-monitoring \
  --metadata enable-oslogin=TRUE,block-project-ssh-keys=TRUE

gcloud compute resource-policies create snapshot-schedule guildnet-daily \
  --region $REGION --daily-schedule --start-time 04:00 \
  --max-retention-days 14 --on-source-disk-delete keep-auto-snapshots
gcloud compute disks add-resource-policies guildnet --zone $ZONE \
  --resource-policies guildnet-daily
```

The `cloud-platform` scope only allows what the service account's roles
allow, here the backup bucket. Containers on the VM can reach the
metadata server and get the account's token; desks can't (egress refuses
link-local addresses). Snapshots contain `.env` and so every secret:
grant `compute.snapshots.*` and `compute.disks.*` to as few people as
possible.

Point both DNS `A` records at the address ([requirements.md](requirements.md#dns)).

## 3. Install

SSH in through IAP:

```bash
gcloud compute ssh guildnet --zone $ZONE --tunnel-through-iap
```

Install Docker Engine from Docker's repository
([Docker's steps for Ubuntu](https://docs.docker.com/engine/install/ubuntu/)):

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin age
sudo usermod -aG docker $USER
exit
```

SSH in again (for the `docker` group), then get the release, keeping a
link to the one in use, and start it ([install.md](install.md) steps 2–5):

```bash
curl -fsSL https://github.com/guildnet-chat/guildnet/archive/refs/tags/26.10.2.tar.gz | tar -xz
ln -sfn guildnet-26.10.2 guildnet
cd guildnet/deploy/selfhost
sh init.sh chat.example.com hq
docker compose pull
docker compose up -d
docker compose logs platform | grep 'setup code'
```

Ubuntu's cloud images install security updates unattended, but only from
Ubuntu's archives. To update Docker the same way (each update restarts
Docker, and so the stack, for a few seconds):

```bash
echo 'Unattended-Upgrade::Allowed-Origins:: "Docker:${distro_codename}";' \
  | sudo tee /etc/apt/apt.conf.d/51unattended-upgrades-docker
```

## 4. Off-site backups

Make an age key pair on your own machine, not the VM, and keep the private
key (`guildnet-backups.key`) offline: the VM gets only the public half, so
nothing on it can read old backups.

```bash
age-keygen -o guildnet-backups.key   # prints "Public key: age1..."
```

On the VM, save the public key and the bucket's name, then the script that
backs up, encrypts, uploads, and keeps a week of local copies, run nightly
at 03:00:

```bash
echo 'age1...' > ~/backup-recipient.txt
echo 'gs://my-project-guildnet-backups' > ~/backup-bucket.txt
cat > ~/guildnet-backup.sh <<'EOF'
#!/bin/sh
set -eu
export PATH="/snap/bin:$PATH" # gcloud, on Ubuntu's images
cd "$HOME/guildnet/deploy/selfhost"
sh backup.sh >/dev/null
b=$(ls -1 backups | tail -n 1)
tar -cf - -C backups "$b" | age -R "$HOME/backup-recipient.txt" \
  | gcloud storage cp - "$(cat "$HOME/backup-bucket.txt")/$b.tar.age"
find backups -mindepth 1 -maxdepth 1 -mtime +7 -exec rm -r {} +
EOF
chmod 700 ~/guildnet-backup.sh
(crontab -l 2>/dev/null; echo '0 3 * * * $HOME/guildnet-backup.sh') | crontab -
```

Run the script once by hand, and check the object appears
(`gcloud storage ls gs://<bucket>` from your machine).

To restore onto a new VM (steps 1–3 without `init.sh`): download the
backup with your own account, copy it and the private key to the VM
(`gcloud compute scp --tunnel-through-iap`), and decrypt it there; delete
the key from the VM afterwards. Then run `restore.sh`
([operations.md](operations.md#restore)):

```bash
mkdir -m 700 ~/restore
age -d -i guildnet-backups.key <time>.tar.age | tar -xf - -C ~/restore
sh restore.sh ~/restore/<time>
```

## 5. Desks

On `e2-standard-4` with a 50 GB disk, follow [desks.md](desks.md): gVisor
and the desks' firewall install on Ubuntu 24.04 as written there.

## Upgrading

As in [operations.md](operations.md#upgrading), from the home directory,
then move the link:

```bash
ln -sfn guildnet-26.11.0 guildnet
```
