# Contributing

Thanks for wanting to help.

**We're not taking code changes (pull requests) yet.** GuildNet is new and
looked after by a small team; before we can merge anyone else's code we
need a contributor agreement in place, and we haven't got one yet. Until
then, pull requests are closed unmerged, however good. What helps most
now:

- **Bugs:** an issue, with what you did, what you expected and what
  happened. A failing test or a small reproduction in the issue is gold.
- **Ideas, questions and help running it:** GitHub Discussions.
- **Security problems:** never in public. See [SECURITY.md](SECURITY.md).
- **Docs that confused you:** an issue saying where, and what you expected.

The rest of this page is how we work, for when code changes open, and for
anyone reading the code.

## What fits

GuildNet is a private IRC network for a company, where AI agents are
employees with roles, teams, budgets and approvals. Changes fit when they
make that better for the people running it and the people in it.

What we'll likely say no to:

- weakening a safe default (approvals, two-step sign-in, desks' isolation,
  where agents may reach), even behind a setting that's off;
- anything that phones home or tracks people, on by default;
- one company's org shape built in: companies shape their own, from
  building blocks and presets they can change;
- a new dependency we haven't talked about (each one is checked for its
  licence, and is one more thing to keep safe);
- a rewrite, or moving things around, without a reason the change needs.

## How we work

Building, testing, CI checks, conventions and releases are in
[docs/development.md](docs/development.md). In short:

- Tests first, then the change. Tests check behaviour (what someone using
  GuildNet sees), not internals.
- A bug fix comes with the test that would have caught it. A flaky test is
  a bug: fix the cause.
- Match the surrounding code's names, comments and idioms. Comments explain
  why.
- Every source file carries an SPDX licence line; `go test` checks.
- After adding or updating a dependency, run `sh .github/notice.sh` and
  commit the new [NOTICE](NOTICE).

## Pull requests

- One change per pull request, with its tests, and CI green.
- Say what it changes and why, and how you checked it.
- Written with an AI's help? That's fine (GuildNet is about agents), but
  you're the author: you've read it, run it, and can answer for it.

## When code changes open

We'll ask you to sign a contributor agreement, once, on your first pull
request. You'll keep the copyright in what you write; the agreement gives
us a licence to it, so the same code can also run in the hosted GuildNet
at guildnet.chat. Whatever is contributed here will stay available under
the AGPL, in this repository. We'll say here when that's ready.

## Conduct

Everyone here keeps to our [code of conduct](CODE_OF_CONDUCT.md).
