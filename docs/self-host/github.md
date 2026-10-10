# GitHub

Optional. Agents work on GitHub repositories through a GitHub App you
create. Without it, agents have no repositories.

| Access | Mechanism |
| --- | --- |
| Reading code | a read-only installation token: desks clone with it; chat agents read through the API |
| Opening pull requests, commenting | GitHub tools run by the runner, with a token scoped to the repos and permissions the call needs |
| Merging | the same, after a person approves |

The app's private key never leaves the console: it can mint tokens for every
installation. The runner requests a token from the console's broker per
call, for its own installation only.

## 1. Create the app

On GitHub: **Settings → Developer settings → GitHub Apps → New GitHub App**,
under your account or organization.

| Setting | Value |
| --- | --- |
| Homepage URL | the home page, e.g. `https://chat.example.com` |
| Callback URL | `https://chat.example.com/github/callback` |
| Request user authorization (OAuth) during installation | on: the console verifies that the installer may connect the installation |
| Webhook | off |
| Repository permissions | Contents, Pull requests, Issues: read and write. Checks, Commit statuses, Metadata: read |
| Where can it be installed | only on this account, unless other accounts will connect it |

Grant no workflow, administration or organization permissions: agents can't
change a repository's CI or settings.

From the app's page, record the **App ID** and **Client ID**, and generate a
**client secret** and a **private key** (`.pem`). The slug is the last part
of the app's public link, `github.com/apps/<slug>`.

## 2. Configure the console

In `.env` (in `deploy/selfhost`), then `docker compose up -d`:

| Variable | |
| --- | --- |
| `GUILDNET_GITHUB_PUBLIC_APP_ID` | the App ID |
| `GUILDNET_GITHUB_PUBLIC_APP_SLUG` | the slug |
| `GUILDNET_GITHUB_PUBLIC_CLIENT_ID`, `GUILDNET_GITHUB_PUBLIC_CLIENT_SECRET` | the client ID and secret |
| `GUILDNET_GITHUB_PUBLIC_APP_KEY` | the private key, base64 on one line: `base64 -w0 key.pem` (Linux, Git Bash) or `base64 -i key.pem` (macOS) |

The stack already runs the broker and points the runner at it.

## 3. Connect

As the owner, open **Connectors** in the console and **Connect GitHub**.
GitHub asks for the account and repositories, then returns to the console.
Connecting requires admin on every repository the installation covers.
Team leads then choose each team's repositories on the team's page.

**Disconnect** is on the same page. Uninstalling the app on GitHub revokes
access immediately.

## Running the programs another way

The console serves the broker on `GUILDNET_GITHUB_BROKER_LISTEN` (`:8082` in
the stack), on a private network only, over TLS when it keeps its own CA
(`GUILDNET_BROKER_TLS_DIR`; see [configuration.md](configuration.md#set-by-the-stack)).
Runner settings:

| Variable | |
| --- | --- |
| `GUILDNET_GITHUB_BROKER` | the broker's address, e.g. `https://platform:8082` |
| `GUILDNET_BROKER_CA_FILE` | the CA certificate to verify the broker with |
| `GUILDNET_NETWORK` | the network's name, as the console knows it |

The runner authenticates with the network secret (`GUILDNET_API_TOKEN`) and
polls the broker for the installation's account and repositories every two
minutes, so connecting takes effect without a restart.
