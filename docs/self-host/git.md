# Git repositories on any host

Optional. Agents work on repositories on any git host (GitLab, Gitea,
Bitbucket, a server of your own) over HTTPS, at their [desks](desks.md):
the runner checks them out there, and agents commit and push branches.
People merge on the host: pull and merge requests, and their status, are
[GitHub](github.md)'s only.

| Access | Mechanism |
| --- | --- |
| Reading code | the runner fetches with the repository's **read** credential and gives the desk the pack: at each readying, and when the agent calls `git_fetch` |
| Pushing a branch | `git_push_branch`, run by the runner with the repository's **push** credential, for that push only |
| Merging | on the host, by people |

No git credential ever reaches a desk, and desks can't reach git hosts at
all: content an agent read can't steer it into sending the company's code
to someone else's repository with a token it was given.

Chat agents (without a desk) get nothing for these repositories: there's no
API to read them through. SSH addresses aren't taken yet; nor are Managed
Agents desks.

## 1. Make the credentials

On the host, for each repository, make two tokens:

| Credential | Make it | Why |
| --- | --- | --- |
| Read | a deploy token (or key) that may only **read** this one repository | the runner reads with it; least privilege all the same |
| Push | a token that may push to this repository (a project access token, a bot account's) | the runner pushes with it; the console tries it can push |

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
if a GitHub repository has its name), with `origin/*` as the host has its
branches. `git fetch` can't reach the host from there: the agent calls
`git_fetch`, then rebases or merges as usual. It commits on a branch of its
own and calls `git_push_branch`; what the host says back (often a link to
open a merge request) goes back to the agent. Each push is in the org log.

## Reaching the host

The runner fetches and pushes, and the console tries credentials, from
their own containers; desks never reach git hosts. Git dependencies a
project pulls straight from a git host (`git+https://` in pip, `github:` in
npm) don't install at a desk: use the registries' releases.

Git hosts must be on the internet. A git server on your own network is
reached only when named in `GUILDNET_GIT_PRIVATE_HOSTS` (host names or
addresses, comma-separated), which the console and the runner read
([configuration.md](configuration.md#git-repositories)).

## Residual risk

- Code read in one of the team's repositories can be pushed to another of
  them without asking, as between GitHub repositories: a push to the team's
  own code isn't a write somewhere else. A write to a connector (Linear,
  say) after reading code still asks a person.
- An agent can push any branch of its own, with anything in it. Review
  before merging.
