# Git repositories on any host

Optional. Agents work on repositories on any git host (GitLab, Gitea,
Bitbucket, a server of your own) over HTTPS, at their [desks](desks.md):
they clone, commit, and push branches. People merge on the host: pull and
merge requests, and their status, are [GitHub](github.md)'s only.

| Access | Mechanism |
| --- | --- |
| Reading code | the repository's **read** credential, which the desk is handed on each readying |
| Pushing a branch | `git_push_branch`, run by the runner with the repository's **push** credential, for that push only. It never reaches a desk |
| Merging | on the host, by people |

Chat agents (without a desk) get nothing for these repositories: there's no
API to read them through. SSH addresses aren't taken yet; nor are Managed
Agents desks.

## 1. Make the credentials

On the host, for each repository, make two tokens:

| Credential | Make it | Why |
| --- | --- | --- |
| Read | a deploy token (or key) that may only **read** this one repository | desks can reach it: anything running at a desk could copy it |
| Push | a token that may push to this repository (a project access token, a bot account's) | only the runner has it; the console tries it can push |

A public repository needs no read credential; one agents only read needs no
push credential.

Protect the default branch on the host (no direct pushes, merges by
review), so only people move it. Agents push only to branches named after
themselves (`kai/fix-build`), never the default branch, and never over
commits they don't have.

| Host | Read credential | Push credential | User name |
| --- | --- | --- | --- |
| GitLab | deploy token, `read_repository` | project access token, `write_repository`, Developer | the deploy token's user name; any for the access token |
| Gitea, Forgejo | an access token with `read:repository`, of a user who may only read it | an access token with `write:repository` | the token's user |
| Bitbucket Cloud | repository access token, Read | repository access token, Write | `x-token-auth` |

## 2. Add the repository

As the network's owner, open **Connectors** in the console. Under **Git
repositories**, give:

- a name (what teams know it by, e.g. `infra`): lowercase letters, digits,
  dots, dashes and underscores;
- its `https://` address, as you'd clone it;
- each credential's user name (if the host wants one) and token.

Both credentials are tried before the repository's added: the read one
must read, the push one must be allowed to push. A read credential that can
push too is taken, with a warning. **Test** tries them again; adding one with
the same name replaces it (to change a token, say). The owner is emailed
whenever a repository is added, changed or removed.

The tokens are sealed in the console's database, bound to the repository's
address: one moved to another address there doesn't open.

## 3. Give it to teams

A team's leads tick it on the team's page (`git:infra` beside GitHub's
repositories), or on IRC: `TEAM SET Engineering REPOS web,git:infra`. A team
inside another uses the outer team's unless it names its own.

At a desk, the repository is in `/workspace/<name>` (`/workspace/git-<name>`
if a GitHub repository has its name). The agent commits on a branch of its
own and calls `git_push_branch`; what the host says back (often a link to
open a merge request) goes back to the agent. Each push is in the org log.

## Reaching the host

Desks reach a git host through `egress` with their network's **egress
pass**: a token the console signs, naming the hosts of that network's
repositories, good for 15 minutes. The proxy opens those hosts for that
pass only; every other desk still reaches only `GUILDNET_DESK_ALLOWED`. The
console tries credentials, and the runner pushes, from their own containers.

Git hosts must be on the internet. A git server on your own network is
reached only when named in `GUILDNET_GIT_PRIVATE_HOSTS` (host names or
addresses, comma-separated), which the console, the runner and `egress`
read ([configuration.md](configuration.md#git-repositories)); with desks on
a host of their own ([desks-host.md](desks-host.md)), set it in that host's
`.env` too.

## Residual risk

- The read credential is at the desk. Anything running there can copy it
  and read the repository elsewhere: keep it read-only and to one
  repository.
- Code read in one of the team's repositories can be pushed to another of
  them without asking, as between GitHub repositories: a push to the team's
  own code isn't a write somewhere else. A write to a connector (Linear,
  say) after reading code still asks a person.
- An agent can push any branch of its own, with anything in it. Review
  before merging.
