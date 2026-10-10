# AWS: EC2

> **Untested.** Written from AWS's documentation and the tested
> [Compute Engine guide](compute-engine.md), but not yet run end to end.
> Report problems in the repository's issues.

The Compose stack ([install.md](install.md)) on one EC2 instance, with an
Elastic IP, a security group that admits only the stack's ports, access
through Systems Manager (no SSH), IMDSv2, daily EBS snapshots, and nightly
encrypted backups in an S3 bucket with Object Lock. ECS isn't used: it
doesn't run Compose files, and desks need sandboxd to drive the host's
Docker with gVisor, which ECS doesn't manage.

| Resource | Setting |
| --- | --- |
| Instance | `t3.large` (2 vCPU, 8 GB) or `m7i.large`; `t3.xlarge` with desks. Ubuntu 24.04 LTS, x86-64 |
| Volume | 30 GB `gp3`, encrypted; 50 GB with desks |
| Address | Elastic IP |
| Security group | tcp 80, 443, 6697 and udp 443 from anywhere; nothing else (no SSH) |
| Access | Systems Manager Session Manager |
| Metadata | IMDSv2 required, hop limit 1: containers can't reach it |
| Instance role | Systems Manager, and adding objects to the backup bucket |
| Backups | daily EBS snapshots (Data Lifecycle Manager, 14 days); nightly `backup.sh`, encrypted with [age](https://age-encryption.org), to S3 with Object Lock (30 days) |

About $67/month in `us-east-1` on demand (instance, volume, address),
before data transfer and snapshot storage.

## 1. Account resources

From a machine with the [AWS CLI](https://aws.amazon.com/cli/) and the
[Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html),
as an administrator of the account. Uses the default VPC.

```bash
export AWS_REGION=us-east-1
BUCKET=my-company-guildnet-backups
VPC=$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
```

Elastic IP and security group:

```bash
aws ec2 allocate-address --domain vpc --tag-specifications 'ResourceType=elastic-ip,Tags=[{Key=Name,Value=guildnet}]'
SG=$(aws ec2 create-security-group --group-name guildnet --description "GuildNet edge" --vpc-id $VPC --query GroupId --output text)
for p in tcp:80 tcp:443 udp:443 tcp:6697; do
  aws ec2 authorize-security-group-ingress --group-id $SG \
    --ip-permissions "IpProtocol=${p%%:*},FromPort=${p#*:},ToPort=${p#*:},IpRanges=[{CidrIp=0.0.0.0/0}]"
done
```

Backup bucket, with Object Lock: nothing can delete or overwrite a backup
for 30 days, including the instance.

```bash
aws s3api create-bucket --bucket $BUCKET --object-lock-enabled-for-bucket \
  $( [ "$AWS_REGION" = us-east-1 ] || echo --create-bucket-configuration LocationConstraint=$AWS_REGION )
aws s3api put-public-access-block --bucket $BUCKET \
  --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-object-lock-configuration --bucket $BUCKET \
  --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"COMPLIANCE","Days":30}}}'
```

Instance role: Systems Manager, and `s3:PutObject` on the bucket only.

```bash
aws iam create-role --role-name guildnet-instance --assume-role-policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
aws iam attach-role-policy --role-name guildnet-instance \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
aws iam put-role-policy --role-name guildnet-instance --policy-name backups --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"s3:PutObject\",\"Resource\":\"arn:aws:s3:::$BUCKET/*\"}]}"
aws iam create-instance-profile --instance-profile-name guildnet-instance
aws iam add-role-to-instance-profile --instance-profile-name guildnet-instance --role-name guildnet-instance
```

## 2. The instance

```bash
AMI=$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
ID=$(aws ec2 run-instances --image-id $AMI --instance-type t3.large \
  --security-group-ids $SG --iam-instance-profile Name=guildnet-instance \
  --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=30,VolumeType=gp3,Encrypted=true}' \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=guildnet}]' 'ResourceType=volume,Tags=[{Key=guildnet-snapshots,Value=daily}]' \
  --query 'Instances[0].InstanceId' --output text)
aws ec2 wait instance-running --instance-ids $ID
ALLOC=$(aws ec2 describe-addresses --filters Name=tag:Name,Values=guildnet --query 'Addresses[0].AllocationId' --output text)
aws ec2 associate-address --instance-id $ID --allocation-id $ALLOC
```

Daily snapshots of volumes tagged `guildnet-snapshots=daily`, kept 14 days
(the default Data Lifecycle Manager role must exist:
`aws dlm create-default-role`):

```bash
aws dlm create-lifecycle-policy --description "GuildNet daily" --state ENABLED \
  --execution-role-arn arn:aws:iam::$(aws sts get-caller-identity --query Account --output text):role/AWSDataLifecycleManagerDefaultRole \
  --policy-details '{"ResourceTypes":["VOLUME"],"TargetTags":[{"Key":"guildnet-snapshots","Value":"daily"}],"Schedules":[{"Name":"daily","CreateRule":{"Interval":24,"IntervalUnit":"HOURS","Times":["04:00"]},"RetainRule":{"Count":14},"CopyTags":true}]}'
```

Snapshots contain `.env` and so every secret: limit who can read them and
the bucket.

Point both DNS `A` records at the Elastic IP ([requirements.md](requirements.md#dns)).

## 3. Install

Open a shell through Session Manager, as the `ubuntu` user:

```bash
aws ssm start-session --target $ID
sudo -iu ubuntu
```

Install Docker Engine as in the [Compute Engine guide](compute-engine.md#3-install)
(the same commands, Ubuntu 24.04), and the AWS CLI for backups:

```bash
sudo snap install aws-cli --classic
```

Then, as `ubuntu` after logging in again for the `docker` group, get the
release and start it (Compute Engine guide step 3, from `curl`).

## 4. Off-site backups

As in the [Compute Engine guide](compute-engine.md#4-off-site-backups),
with the bucket as `s3://<bucket>` in `~/backup-bucket.txt` and the upload
line using `aws s3 cp` instead of `gcloud storage cp`:

```bash
  | aws s3 cp - "$(cat "$HOME/backup-bucket.txt")/$b.tar.age"
```

The instance role can add objects, not read or delete them, and Object
Lock keeps each for 30 days whatever anyone's permissions.

To restore onto a new instance (steps 1–3 without `init.sh`), as in the
Compute Engine guide: download the backup with your own account
(`aws s3 cp`), copy it and the private key to the instance, decrypt it
there, delete the key, and run `restore.sh`.

## 5. Desks

On `t3.xlarge` with a 50 GB volume, follow [desks.md](desks.md). gVisor
needs no nested virtualization (it runs as `runsc` with the `systrap`
platform), so any instance type works.

## Upgrading

As in the [Compute Engine guide](compute-engine.md#upgrading).
