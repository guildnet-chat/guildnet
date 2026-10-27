# AWS: Fargate

> **Untested.** `deploy/aws-fargate/guildnet.yaml` passes `cfn-lint` and
> GuildNet's own checks against the Compose stack, but hasn't been
> deployed. Report problems in the repository's issues. The tested
> alternative on AWS is [EC2](ec2.md); on Google Cloud, [GKE](gke.md).

The stack in one ECS task on Fargate, from one CloudFormation template:
Caddy, the console, Ergo and the runner, with Postgres on RDS and their
files on EFS, behind a Network Load Balancer on two Elastic IPs. Desks run
on an EC2 instance of their own ([desks-host.md](desks-host.md)).

| Resource | Setting |
| --- | --- |
| Network | a VPC of its own: two public subnets (the task, with a public address for its way out, and the load balancer), two private ones (RDS, EFS) |
| Task | one (the service stops it before starting a replacement), 1 vCPU, 2 GB; every container with a read-only root filesystem and no capabilities (but `CHOWN` for the one that hands Caddy its folder), and non-root but for two that set up the task's own folders and exit; Caddy binds 80 and 443 as its own user (`net.ipv4.ip_unprivileged_port_start`, which applies to the whole task) |
| Addresses | in one task the containers share localhost: the stack's addresses (`platform:8080`, `ergo:6667`, …) are localhost's, and the console reapplies them at start |
| Database | RDS for PostgreSQL, `db.t4g.small`, private, encrypted, TLS required and checked against RDS's CA, 14 days of backups, deletion protection |
| Files | EFS, encrypted, TLS only, an access point per program setting who owns its files; AWS Backup on |
| Edge | NLB on two Elastic IPs, TCP 80, 443 and 6697 to Caddy, visitors' addresses kept |
| Settings | one Secrets Manager secret, read by ECS into each container's environment |

About $75/month in `us-east-1` (Fargate, RDS, EFS, NLB, addresses), plus
the desks host.

## 1. Requirements

The [AWS CLI](https://aws.amazon.com/cli/), `jq`, and an account where you
may create the stack's resources and IAM roles.

## 2. Settings

Make the install's settings with the Compose stack's `init.sh`, then write
them, with two values Ergo reads in its own form, to a secret:

```bash
cd guildnet-26.10.3/deploy/selfhost
sh init.sh chat.example.com hq
cd ../aws-fargate
( set -a; . ../selfhost/.env; set +a
  jq -n '
    {GUILDNET_PLATFORM_OLD_KEYS:"", GUILDNET_SMTP_ADDR:"", GUILDNET_SMTP_USERNAME:"", GUILDNET_SMTP_PASSWORD:"",
     GUILDNET_SMTP_SECURITY:"", GUILDNET_MAIL_FROM:"", GUILDNET_MAIL_REPLY_TO:"", GUILDNET_RESEND_KEY:"",
     GUILDNET_GITHUB_PUBLIC_APP_ID:"", GUILDNET_GITHUB_PUBLIC_APP_SLUG:"", GUILDNET_GITHUB_PUBLIC_CLIENT_ID:"",
     GUILDNET_GITHUB_PUBLIC_CLIENT_SECRET:"", GUILDNET_GITHUB_PUBLIC_APP_KEY:"", GUILDNET_BRAND_NAME:"",
     GUILDNET_BRAND_TAGLINE:"", GUILDNET_BRAND_HELLO:"", GUILDNET_BRAND_SUPPORT:"", GUILDNET_BRAND_PRIVACY:"",
     GUILDNET_BRAND_SECURITY:"", GUILDNET_SOURCE:"", GUILDNET_CHAT_SOURCE:"", GUILDNET_SANDBOX_URL:"",
     GUILDNET_SANDBOX_KEY:"", GUILDNET_SANDBOX_NAMES:"", GUILDNET_GIT_PRIVATE_HOSTS:"", GUILDNET_SENTRY_DSN:"", GUILDNET_ENVIRONMENT:"",
     GUILDNET_MANAGED:"", GUILDNET_DESK_BUDGET_CENTS:""}
    + (env | with_entries(select(.key | startswith("GUILDNET_"))))
    | .ERGO__API__BEARER_TOKENS = ([.GUILDNET_ERGO_API_TOKEN] | tojson)
    | .ERGO__ACCOUNTS__JWT_AUTH__TOKENS = ([{algorithm: "hmac", key: .GUILDNET_CHAT_KEY,
        "account-claims": ["account"], "validate-aud": [.GUILDNET_NETWORK]}] | tojson)' > settings.json )
chmod 600 settings.json
aws secretsmanager create-secret --name guildnet --secret-string file://settings.json --query ARN --output text
shred -u settings.json
```

Every setting the template names must be in the secret, even if empty:
ECS won't start a task missing one. Keep `../selfhost/.env` somewhere safe
(a secret manager): its secrets encrypt the install's data. Optional
settings (email, GitHub, branding) go in `.env` before this step, values
with spaces quoted (`GUILDNET_MAIL_FROM="GuildNet <guildnet@example.com>"`),
as in [configuration.md](configuration.md). Branding files aren't
supported here.

## 3. The stack

With the secret's ARN, create the stack with no task running yet (the
console can't start until its database exists):

```bash
aws cloudformation deploy --stack-name guildnet --template-file guildnet.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides Domain=chat.example.com Network=hq Release=26.10.3 \
    SettingsSecret=<secret ARN> Running=0
aws cloudformation describe-stacks --stack-name guildnet --query 'Stacks[0].Outputs' --output table
```

Point both DNS names' `A` records at both of the `Addresses` output's
addresses ([requirements.md](requirements.md#dns)).

## 4. Database, then start

Run the `DatabaseInitCommand` output, which makes the console's and the
runner's databases and logins on RDS as its admin (safe to run again), and
wait for the task to stop with exit code 0
(`aws logs tail /guildnet --follow`). Then run the task:

```bash
aws cloudformation deploy --stack-name guildnet --template-file guildnet.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides Running=1 \
  Domain=chat.example.com Network=hq Release=26.10.3 SettingsSecret=<secret ARN>
aws logs tail /guildnet --since 10m --filter-pattern '"setup code"'
```

Open `https://chat.example.com` and set the network up
([install.md](install.md#5-first-run-setup)).

## Operating

| Task | How |
| --- | --- |
| Logs | `aws logs tail /guildnet --follow` (streams `guildnet/<container>/…`) |
| Changed settings | update the secret (`aws secretsmanager put-secret-value`), then `aws ecs update-service --cluster <cluster> --service guildnet --force-new-deployment` |
| Locked-out owner, or signing a desks host's certificate | deploy with `AllowExec=true`, then `aws ecs execute-command --cluster <cluster> --task <task> --container platform --interactive --command "/platform reset-owner"`; deploy with `AllowExec=false` afterwards |
| Desks | [desks-host.md](desks-host.md) on EC2 in the stack's VPC, letting in 8443 from the `TaskSecurityGroup` output; `--command "/platform sandbox-cert"` as above, pasting `sandboxd.csr` then Ctrl-D, and copying the certificate it prints |
| Upgrading | read the release notes, then deploy the new release's template with `Release=<new release>`; the task is replaced, never doubled |
| Backups | RDS keeps 14 days of automated backups; AWS Backup covers EFS. Restoring a Compose backup into Fargate isn't covered yet |
