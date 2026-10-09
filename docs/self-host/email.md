# Email

Optional. Without it the install runs normally, and nothing below is sent.

| Sent | To |
| --- | --- |
| Invitations | the invitee |
| Approvals waiting | people who opted in on their account page |
| Security notices (model key added or changed, GitHub connected, …) and the daily spending cap reached | the owner's alert address |

Configure in `.env` (in `deploy/selfhost`), then `docker compose up -d`.
The console's **Server** page shows whether email is on and sends a test.

| Variable | |
| --- | --- |
| `GUILDNET_MAIL_FROM` | the sender, e.g. `GuildNet <hello@example.com>` |
| `GUILDNET_MAIL_REPLY_TO` | optional; required with Resend |

Set either SMTP or Resend, not both.

## SMTP

| Variable | |
| --- | --- |
| `GUILDNET_SMTP_ADDR` | `host:port`, e.g. `smtp.example.com:587` |
| `GUILDNET_SMTP_USERNAME`, `GUILDNET_SMTP_PASSWORD` | if the server requires authentication |
| `GUILDNET_SMTP_SECURITY` | `starttls` (default; usually 587), `tls` (usually 465), or `none` |

Credentials are never sent without TLS. `none` is for a server on the same
host or private network, without credentials.

A provider's relay (Postmark, SES, Mailgun, Fastmail, Google Workspace):

```sh
GUILDNET_MAIL_FROM='GuildNet <hello@example.com>'
GUILDNET_SMTP_ADDR=smtp.example.com:587
GUILDNET_SMTP_USERNAME=…
GUILDNET_SMTP_PASSWORD=…
```

A server on the host, listening on Docker's bridge address (`172.17.0.1`
on Linux; `host.docker.internal` on Docker Desktop):

```sh
GUILDNET_SMTP_ADDR=172.17.0.1:25
GUILDNET_SMTP_SECURITY=none
```

### Testing with Mailpit

[Mailpit](https://mailpit.axllent.org) captures all mail locally. Start it
after the stack (which creates the `guildnet_private` network):

```sh
docker run -d --name mailpit --network guildnet_private -p 127.0.0.1:8025:8025 axllent/mailpit
```

```sh
GUILDNET_SMTP_ADDR=mailpit:1025
GUILDNET_SMTP_SECURITY=none
```

The mailbox is at `http://localhost:8025` on the host (SSH-tunnel from
elsewhere). Remove it (`docker rm -f mailpit`) before `docker compose down`,
which can't remove the network while Mailpit is attached.

## Resend

Set `GUILDNET_RESEND_KEY` (a sending-only key for the `From` domain)
instead of `GUILDNET_SMTP_ADDR`. `GUILDNET_MAIL_REPLY_TO` is required.
