# deploy/aws-fargate

The stack in one ECS task on Fargate, with Postgres on RDS and the files on
EFS, behind a Network Load Balancer: one CloudFormation template,
`guildnet.yaml`. **Untested**; it passes `cfn-lint` and
`deploy/selfhost/fargate_test.go`'s checks against the Compose stack.
Guide: [docs/self-host/fargate.md](../../docs/self-host/fargate.md).

| Parameter | |
| --- | --- |
| `Release`, `Registry` | which images run |
| `Domain`, `Network` | as `init.sh`'s arguments |
| `SettingsSecret` | the Secrets Manager secret with the install's settings, as JSON |
| `Running` | `0` until the database is set up, then `1`; never more |
| `AllowExec` | ECS Exec into the task, only while needed |
| `PostgresVersion` | RDS's major version |