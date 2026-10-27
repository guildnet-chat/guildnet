# Developing GuildNet

How to build, run, test and release GuildNet's open-source code.
[CONTRIBUTING.md](../CONTRIBUTING.md) sets the house rules; this page is
the reference behind them.

## Prerequisites

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | the `go` line in `go.mod` (1.27.1) | everything; CI reads the version from `go.mod` |
| Docker | Engine 28 or later | tests against real Ergo and Postgres; desk, image and stack tests (`GUILDNET_DOCKER_TESTS=1`); building images; gitleaks |
| Node | 24 (guildnet-chat's CI) | the web chat |
| git | any recent | the licence-header test reads `git ls-files`; the git-host tests run a real git server (`internal/gittest`, skipped without git) |
| `sh` | POSIX shell | the self-host script tests and `.github/notice.sh`; Git Bash on Windows |

Without Docker, `go test ./...` still passes: tests that need it skip.

## Repository layout

GuildNet is two repositories, cloned side by side:

| Repository | What |
| --- | --- |
| `guildnet` | Go module `github.com/guildnet-chat/guildnet`: the console, the runner, sandboxd, the images, the self-host stack, the docs |
| `guildnet-chat` | the web chat, a fork of gamja (Preact, Parcel, npm). A separate AGPL program: the console serves its built files |

The console's image builds the chat from the second repository, passed as
the `chat-src` build context:

```bash
docker build -f deploy/platform/Dockerfile --build-context chat-src=../guildnet-chat .
```

The self-host stack does the same through `additional_contexts`, from
`GUILDNET_CHAT_SRC` (default `../../../guildnet-chat`, relative to
`deploy/selfhost`).

### Public API

Packages outside `internal/` are the Go API. It is version 0 and may
change.

| Package | What |
| --- | --- |
| `app/platform`, `app/runner` | run the console or a runner from their settings; `Config` plugs in what a build adds |
| `platform` | the console; an `Extension` adds pages, sections, routes and policies through `Hooks` |
| `platformtest` | the console's test harness, also for extensions' tests |
| `runner` | a network's runner: each agent on IRC as its own user, and when each wakes |
| `config` | loads the runner config and persona files |
| `models` | the model catalogue: providers, costs, tiers |
| `ledger` | model spend per agent per UTC day, and budgets |
| `netspec` | what the console and whatever runs a network say to each other |
| `templates` | what a new network can start from |
| `charts` | chart descriptions for the console's pages |
| `limit` | slows down guessing and floods |
| `mail` | GuildNet's own email |
| `monitor` | fault reports to Sentry |
| `pki` | GuildNet's own CA, for TLS on internal hops |
| `pgtest` | a throwaway Postgres in Docker, for tests |

### Internal packages

| Package | What |
| --- | --- |
| `internal/agent` | what an agent is told, what comes of waking it, and its tools |
| `internal/brain` | calls each agent's model at its provider |
| `internal/chat` | recent conversation and topics, in memory |
| `internal/connectors` | Linear, Notion, Sentry and Atlassian as MCP tools |
| `internal/desk` | desks of GuildNet's own: six tools run in a sandbox |
| `internal/egress` | calls to customer-chosen addresses: HTTPS, public addresses only |
| `internal/ergotest` | a throwaway real Ergo in Docker, for tests |
| `internal/github` | the GitHub App integration |
| `internal/gitremote` | a network's git repositories on other hosts: desks' checkouts, and `git_push_branch` |
| `internal/gittest` | a real git server over HTTPS, for tests |
| `internal/gitwire` | git's smart HTTP protocol: list a repository's refs, push one branch |
| `internal/ircfake` | an in-process fake IRC server, for tests |
| `internal/managed` | agents on Anthropic's Managed Agents |
| `internal/org` | the org chart: teams, people, agents |
| `internal/roles` | the role catalogue |
| `internal/sandbox` | sandboxd: one container per agent's workspace |
| `internal/workweek` | deadline arithmetic in working time |

Also: `cmd/`, `deploy/` (images, the self-host stack, the cloud
deployments), `docs/`, `templates/`,
`guildnet.yaml` and `personas/` (a sample network), `examples/`
(Apache-2.0).

## Building

### Binaries

| Command | What |
| --- | --- |
| `cmd/platform` | the console: sign-in, each network's console, web chat |
| `cmd/runner` | a network's runner: its agents, GuildServ, the ledger, its API |
| `cmd/sandboxd` | desks: `serve` (the API), `proxy` (desks' only way out), `keygen` (a key pair, printed as settings) |
| `cmd/peek` | prints a Managed Agents desk session's events (a debugging aid) |

```bash
go build ./cmd/platform ./cmd/runner ./cmd/sandboxd ./cmd/peek
```

The images build with `CGO_ENABLED=0 go build -trimpath` and set the
release with `-ldflags`:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/guildnet-chat/guildnet/monitor.release=26.10.0" -o runner ./cmd/runner
```

The console also takes `-X main.chatCommit=<guildnet-chat commit>` for the
chat's Source link.

### Images

Build from the repository root unless noted. Each Go image takes the
`GUILDNET_RELEASE` build argument, which fault reports and the console's
**Server** page show.

```bash
docker build -f deploy/platform/Dockerfile --build-context chat-src=../guildnet-chat \
  --build-arg GUILDNET_RELEASE=26.10.0 --build-arg GUILDNET_CHAT_COMMIT=<commit> .
docker build -f deploy/runner/Dockerfile --build-arg GUILDNET_RELEASE=26.10.0 .
docker build -f deploy/sandboxd/Dockerfile --build-arg GUILDNET_RELEASE=26.10.0 .
docker build -t guildnet-desk:latest deploy/desk
docker build deploy/caddy
docker build deploy/ergo
docker build deploy/postgres
```

The platform image builds the chat with `npm ci --ignore-scripts` (no
package's install scripts run) and `npm run build` on
`node:24-alpine` and serves it from `/chat`. The runner compiles with
`-p 1` to fit a 2 GB Docker. sandboxd's image runs both `serve` and
`proxy`. The caddy, ergo, postgres and desk images build from their own
directories; sandboxd runs desks from `GUILDNET_DESK_IMAGE` (default
`guildnet-desk:latest`; the self-host stack sets the release's desk image).

## Running locally

The self-host stack in `deploy/selfhost` runs everything: Caddy, the
console, Ergo, the runner, Postgres, and optionally sandboxd and egress.

```bash
cd deploy/selfhost
sh init.sh chat.example.com hq
GUILDNET_RELEASE=$(git describe --tags --always) \
GUILDNET_CHAT_COMMIT=$(git -C ../../../guildnet-chat rev-parse HEAD) \
  docker compose up -d --build
docker compose logs platform | grep 'setup code'
```

`init.sh` writes `.env` (mode 600) with every secret, and refuses to
overwrite one. `desks.sh` turns desks on. The stack needs a real domain
whose DNS points at the machine: Caddy gets certificates from Let's Encrypt
at start, and the console sets secure cookies. On a machine without a
domain, use the test harnesses below to exercise the console and runner.

The programs also read development settings for running pieces by hand
([configuration.md](self-host/configuration.md#not-for-a-self-hosted-install)):

| Setting | Effect |
| --- | --- |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY` | the runner's model keys, without the console's broker |
| `GUILDNET_CONFIG` or `-config` | the runner's settings file (default `guildnet.yaml`) |
| `GUILDNET_LOCAL_NETWORK`, `GUILDNET_LOCAL_RUNNER_URL`, `GUILDNET_LOCAL_ERGO_URL`, `GUILDNET_LOCAL_CHAT_URL` | the console with one network set up by hand (also needs `GUILDNET_API_TOKEN`, `GUILDNET_ERGO_API_TOKEN`) |
| `GUILDNET_DEV_SIGNIN=on` | sign in on the home page by email alone; refused with `GUILDNET_SECURE_COOKIES` |
| `GUILDNET_SANDBOX_URL`, `GUILDNET_SANDBOX_KEY` on the runner | a runner that signs its own desk tokens |

`guildnet.yaml` is a sample network (Acme) for `go run ./cmd/runner`; it
expects an IRC server at `127.0.0.1:6667`. Without a database URL, the
runner and the console keep their data in memory. The console defaults to
`guildnet.localhost` on `:8080`.

## Tests

```bash
go test ./...
GUILDNET_DOCKER_TESTS=1 go test -race ./...
```

| Needs | Tests | Without it |
| --- | --- | --- |
| nothing | almost everything: the runner on `ircfake`, the console through `platformtest`, script tests, docs and licence tests | runs |
| Docker running | Postgres stores (`pgtest`), real Ergo (`internal/ergotest`) | skip |
| Docker and `GUILDNET_DOCKER_TESTS=1` | desks' containers (`internal/sandbox`, `internal/desk`), a desk cloning and pushing through egress (`internal/gitremote`), image and stack tests in `deploy/selfhost` | skip |
| live settings | `live_test.go` in `internal/managed` and `internal/github` | skip |

CI runs the second command. Live tests call real services and are off by
default:

| Setting | Runs |
| --- | --- |
| `GUILDNET_LIVE_MANAGED=1` | a real Managed Agents desk; costs a few cents |
| `GUILDNET_LIVE_GITHUB=1`, `GUILDNET_LIVE_CONFIG` | a real GitHub App; opens and closes a real PR. `GUILDNET_LIVE_CONFIG` is a runner config naming repos, with the `GUILDNET_GITHUB_*` settings set |

### The console: platformtest

A `World` is the console with two networks, acme and globex, each a real
runner on a fake IRC server, plus a fake Ergo API, fake model providers
and a movable `Clock`. A `Browser` keeps one network's cookies and posts
forms with the last page's CSRF token.

```go
func TestAWrongPasswordIsRefused(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")

	r := b.SignIn("ada", "guess")

	if r.Status != http.StatusUnauthorized {
		t.Errorf("status %d; want 401", r.Status)
	}
}
```

`b := w.Browser("acme").SignedIn("ada")` signs in or fails the test;
`b.Get(path)` and `b.Post(path, form)` return a `Response`. `Setup`
tweaks change the console's options at each start, for example
`AnyoneCreates`, `WithMail(outbox)`, or an `Extension`:

```go
w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, AnyoneCreates)
```

`NewWorldWithPostgres` and `NewDatabase` need the package's `TestMain` to
call `Main`, which starts a throwaway Postgres when Docker runs:

```go
func TestMain(m *testing.M) { platformtest.Main(m) }
```

`NewSelfHostedWorld` is an install before first-run setup.

### The runner: fake IRC and a scripted model

`runner/harness_test.go` starts `ircfake`, the real runner, and a human on
an IRC client. `fakeBrain` stands in for the model: a test scripts its
replies, tool steps, usage, approvals and failures.

```go
func TestAgentRepliesInChannelWhenMentioned(t *testing.T) {
	brain := &fakeBrain{reply: replyWith("on it, fixing the flaky test now")}
	n := startNetwork(t, brain)

	n.human.say("#engineering", "kai: can you look at the flaky test?")

	n.human.expect("kai's reply in #engineering", fromContaining("kai", "#engineering", "on it"))
	n.human.expectQuiet("a reply from vera", 300*time.Millisecond, from("vera", "#engineering"))
}
```

The harness is in package `runner_test`, not exported.

### pgtest

One Postgres container per test binary; each test gets a fresh database.

```go
var pg *pgtest.Server

func TestMain(m *testing.M) {
	var err error
	pg, err = pgtest.Start()
	if err != nil && !errors.Is(err, pgtest.ErrNoDocker) {
		panic(err)
	}
	code := m.Run()
	if pg != nil {
		pg.Stop()
	}
	os.Exit(code)
}

func TestSomething(t *testing.T) {
	if pg == nil {
		t.Skip("Docker isn't running; skipping Postgres")
	}
	url, err := pg.NewDatabase()
	// ...
}
```

### Self-host scripts

`deploy/selfhost/scripts_test.go` runs the install's scripts on the
machine's own `sh`, with `testdata/fakedocker` (built for the machine) as
`docker`. fakedocker logs each call to `$FAKEDOCKER_LOG` and gives canned
answers; tests check the files written and the calls logged. Without `sh`
they skip, except under CI, where they fail.

```bash
go test -run "Init|Backup|Restore|DesksSh" ./deploy/selfhost/
```

### House rules

- Write the test first, then the change.
- Test behaviour a user sees, not internals.
- Name a test for what is true when it passes
  (`TestEveryPageLinksToTheSource`); its comment says why it matters.
- A bug fix comes with the test that would have caught it.
- A flaky test is a bug: fix the cause, never retry.

## CI checks

`.github/workflows/ci.yml` runs on every pull request and push to `main`.

| Check | Run locally |
| --- | --- |
| vet | `go vet ./...` |
| formatting | `test -z "$(gofmt -l .)"` |
| tests | `GUILDNET_DOCKER_TESTS=1 go test -race ./...` |
| scripts (ubuntu, macos, windows) | `go test -run "Init\|Backup\|Restore\|DesksSh" ./deploy/selfhost/` |
| licences | see below |
| NOTICE | `sh .github/notice.sh && git diff --exit-code NOTICE` |
| secrets | see below |

Licences: no dependency under a forbidden, restricted or unknown licence.

```bash
go install github.com/google/go-licenses/v2@v2.0.1
go-licenses check ./... --ignore github.com/guildnet-chat/guildnet --ignore github.com/segmentio/asm --disallowed_types=forbidden,restricted,unknown
```

`segmentio/asm` is MIT-0, which go-licenses does not know; it is listed by
hand in `.github/notice/head.txt`.

Secrets, across every commit:

```bash
docker run --rm -v "$PWD:/repo" ghcr.io/gitleaks/gitleaks:v8.30.1 git --redact --no-banner --exit-code 1 /repo
```

`go test` covers the rest:

| Test | Checks |
| --- | --- |
| `licence_test.go` | `LICENSE` and `examples/LICENSE` are there; every tracked source file has its SPDX line; `NOTICE` names vendored code, Ergo and the fonts |
| `docs/docs_test.go` | every `GUILDNET_` setting is in `configuration.md`; every table ends with a blank line; every relative link and anchor resolves |
| `deploy/selfhost` | what `compose.yml` exposes, capabilities, networks, volumes |

## Conventions

### Licence headers

Every `.go`, `.sh`, `.sql`, `.js`, `.css`, `.html`, `.py`, `.yml`,
`.yaml`, `Dockerfile` and `Caddyfile` has an SPDX line in its first five
lines.

| File | Line |
| --- | --- |
| Go, JS | `// SPDX-License-Identifier: AGPL-3.0-only` |
| shell, YAML, Python, Dockerfile, Caddyfile | `# SPDX-License-Identifier: AGPL-3.0-only` |
| SQL | `-- SPDX-License-Identifier: AGPL-3.0-only` |
| CSS | `/* SPDX-License-Identifier: AGPL-3.0-only */` |
| HTML template | `{{/* SPDX-License-Identifier: AGPL-3.0-only */ -}}` |

`examples/` uses `Apache-2.0`. `deploy/ergo/ircd.yaml` is
`AGPL-3.0-only AND MIT`. Exempt: `guildnet.yaml`, `personas/`,
`templates/`, and vendored files in `platform/static` (listed in
`licence_test.go`).

### Dependencies

Do not add a dependency without discussing it first: each is checked for
its licence and is one more thing to keep safe. After adding or updating
one, run `sh .github/notice.sh` and commit the new `NOTICE`.

### Comments and copy

Comments say why, in plain words. The console's pages use short sentences
and the words its users use. Match the surrounding code's names and style.

### Migrations

The console's schema is in `platform/migrations`; the runner's org chart
is in `internal/org/migrations`. Both are goose SQL files
(`-- +goose Up`, `-- +goose Down`), embedded with `go:embed`, and applied
by `goose.Up` when `OpenPostgres` runs. Files are named
`NNNNN_name.sql`; add a new one numbered above the highest present, and
never edit one that has shipped. The console also runs one-off upgrades
in Go after migrating, recorded in its `upgrades` table.

A migration adds; it takes away only what a release already out stopped
using. A deploy that hands the console over to a new copy (guildnet.chat
does) runs the old copy for a while on the new schema, so a column or
table the old code reads must outlast it. Stop using it in one release,
then drop or rename it in a migration of a later one, saying which:
`-- After release 26.10.2, which stopped using it.` in its Up section.
`pgtest.ExpandOnly` checks this for every new migration (a drop or rename
without it, or naming a release not out yet, fails the tests).

### Errors and logging

Log with the `*log.Logger` passed in. Report a fault in GuildNet's own
code with `monitor.Problem(logger, format, args...)`: it logs, and sends
to Sentry when `GUILDNET_SENTRY_DSN` is set. The format string groups
reports, so keep it fixed and put details in the arguments. Commands
defer `monitor.Start(monitor.FromEnv(service))()` and `monitor.Recover()`;
HTTP handlers are wrapped with `monitor.Handler`.

## Releases

Releases use CalVer, `YY.0M.N`: `26.10.0` is the first release of October
2026, `26.10.1` the next that month. Tags are plain, with no leading `v`.
Go module tags must be semver, and the module path has no `/v26`
suffix, so these tags are not module versions. Code importing the module
uses a pseudo-version; `go get` resolves a commit or a release tag to one:

```bash
go get github.com/guildnet-chat/guildnet@26.10.0
go get github.com/guildnet-chat/guildnet@<commit>
```

| File | |
| --- | --- |
| `VERSION` | the release; `init.sh` pins an install to it (`GUILDNET_VERSION`) |
| `deploy/platform/chat-release` | the guildnet-chat release (a tag there) the console's image builds in |

To release: on `main`, set `VERSION` (and `chat-release`, after tagging
guildnet-chat if the chat changed) in a commit whose CI passes, then tag
that commit with the same name and push the tag:

```bash
git tag 26.10.0 && git push origin 26.10.0
```

`.github/workflows/release.yml` then checks the tag against `VERSION`, and
that its commit is on main and passed CI (tag only once CI is green),
builds the seven images (linux/amd64) as `deploy/selfhost/compose.yml`
builds them, pushes each to `ghcr.io/guildnet-chat/<image>` tagged with the
release and `latest`, with provenance and an SBOM, signs each with cosign
(keyless: the workflow's GitHub identity) and attests its build, and drafts
the GitHub release naming the chat's release. Edit the draft's notes, then
publish it. `deploy/selfhost/images_test.go` keeps the workflow's image list
in step with the stack's.

Every workflow's actions are pinned to a commit (`release_test.go` checks).
`.github/workflows/scan.yml` rebuilds every image from main each Monday and
fails on a high or critical vulnerability with a fix out: update the base
image's digest or the module, and release.

To rehearse a release first, run the workflow by hand (**Actions →
release → Run workflow**, or `gh workflow run release.yml`): it builds
every image of the release `VERSION` names, as a tag would, and pushes
nothing and drafts nothing.

## The web chat

guildnet-chat is gamja with GuildNet's changes. Use the Node version from
its CI (24).

```bash
npm install
npm run lint
npm test
npm run build
npm start
```

| Script | What |
| --- | --- |
| `lint` | ESLint; CI adds `-- --max-warnings 0` |
| `test` | `node --test "test/*.test.js"` |
| `build` | Parcel, into `dist/` |
| `start` | gamja's development server (`dev-server.js`); `npm start -- <irc host>` proxies WebSockets to an IRC server |
| `clean` | removes `dist/` |

In GuildNet, the chat's config is the console's `/chat/app/config.json`,
served only to signed-in people (`platform/chat.go`): the socket, token
sign-in, autojoin channels from the org chart, AI and BOT labels, member
groups and team repos.

Keep changes close to gamja's code and style, so the fork keeps taking
upstream changes; send fixes unrelated to GuildNet upstream. Dependencies
that ship in the chat must be under a permissive licence:

```bash
node .github/licences.mjs
```

It reads `package-lock.json`, so it needs no install.
