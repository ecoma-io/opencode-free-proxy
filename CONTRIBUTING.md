# Contributing to opencode-free-proxy

Thank you for wanting to contribute. This is a single-maintainer project:
issues and pull requests are both welcome, and both go through the same
gates.

By contributing you agree that your work is licensed under the Apache
License 2.0, and that you have the right to grant that license.

## The behavior contract

[`README.md`](README.md) is the entry point and quick start. The behavior
contract lives across the docs split and the code: the request pipeline,
the model-naming rules, and what the proxy forwards where are in
[`docs/architecture.md`](docs/architecture.md), the config document is
[`docs/configuration.md`](docs/configuration.md), and **who may recover
from what** is [`docs/recovery-semantics.md`](docs/recovery-semantics.md) —
the cross-service contract that makes provider responses terminal at this
proxy and confines egress failover to failures proven to precede the
request. Its terms are implemented in
[`internal/upstream`](internal/upstream) (`Failure`, `ReplaySafe`) and
`internal/router` (the failover decision).
[`AGENTS.md`](AGENTS.md) carries the porting discipline built on top of
it — JS citations on every ported behavior, JS truthiness through
`internal/jsonx`, constants only in `internal/config`, documented
divergences, and fail-open vs fail-closed as part of the contract.

A change that moves documented behavior updates both documents in the same
pull request. A document that lags the code is a defect, not a follow-up.

## Setting up

- **Go ≥ 1.26** — `go.mod` declares the floor, and CI installs with
  `go-version-file: go.mod`, so the module owns its toolchain version.
- **Node ≥ 24 and pnpm ≥ 11** — only for the repository hooks and formatting
  (nothing here ships to npm). `pnpm install` runs `lefthook install`; if a
  repository's hooks did not run for you, it is because that step was
  skipped. Do not skip it.
- `golangci-lint` is optional locally — CI downloads and checksum-pins its
  own copy, so a local copy at any recent v2 works.

## The commands

| Command                                   | What it does                                                                                      |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------- |
| `gofmt -w .`                              | Format; the first half of every change                                                            |
| `go vet ./... && go vet -tags e2e ./e2e/` | Vet; the e2e package needs its own pass behind its build tag                                      |
| `go test -race ./...`                     | The unit suite (the tag-gated e2e package is skipped unscoped)                                    |
| `go test -tags e2e ./e2e/`                | The black-box suite: the compiled server as a subprocess against a fake zen upstream — no network |
| `go build ./cmd/server`                   | Build the binary                                                                                  |
| `pnpm format` / `pnpm format:check`       | Prettier over the docs, workflows, and config files                                               |
| `docker compose up -d --build`            | Run the proxy locally for manual testing                                                          |

## What the hooks do

| Hook         | Commands                                                                   |
| ------------ | -------------------------------------------------------------------------- |
| `pre-commit` | `gofmt -w` over staged `*.go` · prettier over staged docs/workflows/config |
| `commit-msg` | commitlint over the message                                                |
| `pre-push`   | `go test ./...`                                                            |

Bypassing a hook with `--no-verify` is occasionally the right call during a
rebase. It is never the right way to land a change.

## Commit messages

Conventional Commits, enforced by commitlint both on the hook and on the
pull-request title in CI:

```
<type>(<scope>): <subject>
```

- Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore`, `revert`.
- Scopes name the area: `identity`, `logging`, `cloak`, `translate`, `relay`,
  `upstream`, `router`, `routing`, `health`, `caps`, `usage`, `jsonx`,
  `config`, `cmd`, `e2e`, `docs`, `deps`, `ci`, `workspace`, `release`.
  The scope is optional; `deps` and `ci` exist so that dependency-automation
  pull requests pass the same gate as human ones.
- Breaking changes add `!` before the colon and a `BREAKING CHANGE:` footer.
- Subject at most 100 characters; body line length unlimited.

**AI-assisted disclosure.** If a commit was AI-assisted, it carries a
trailer: `Assisted-by: <tool>` or `Generated-by: <tool>`. One trailer per
pull request, on the last commit — merges are squash merges, and the
squashed commit concatenates trailers, so per-commit trailers would
duplicate.

## Tests

- Unit tests live beside the code under `internal/`; the black-box E2E
  suite lives under `e2e/` behind the `e2e` build tag and drives the
  compiled server as a subprocess against a fake zen upstream. It needs
  nothing but Go and never touches the real upstream; the opt-in live suite
  (`E2E_LIVE=1`) is for humans, never for gates.
- A test that only pins the loud direction is not a test. This is a
  free-tier client: prefer the case where a change fails _quietly_ — a 429
  that gets retried or moved to another egress when the contract says fail
  fast, a POST replayed after the request may already have reached the
  provider, a UA that stays stale past its TTL, a fingerprint tool that
  stops being injected, an SSE line dropped in a relay branch nobody
  exercises.
- Golden unit vectors are ported from `9router-src`'s
  `tests/unit/opencode-*.test.js` — when upstream behavior surprises you,
  the JS source is the place to look first.

## Opening a pull request

1. Branch from `main`.
2. Make the change, add the tests, run the commands.
3. Fill the pull-request template honestly — especially "Could this fail
   silently?" Writing "no" is fine when it is true; leaving it blank is not.
4. Keep it focused: one behavior per pull request, docs in the same pass.

**Squash, always.** The pull-request title becomes the squash commit's
subject, so the title itself must be a valid Conventional Commit — CI runs
commitlint on it before anything else matters.

## How a release happens

[release-please](https://github.com/googleapis/release-please) owns
`CHANGELOG.md` and the version tag; do not hand-edit either. Every merged
`feat`/`fix` commit updates an open release pull request; merging that pull
request tags `v<version>` and publishes the Docker image to
`ghcr.io/ecoma-io/opencode-free-proxy` — both the version tag and
`latest`, except that a prerelease never moves `latest`. A
`Release-As: <version>` footer on a commit forces a version once.

## Reporting problems

- Bugs: [the bug report form](.github/ISSUE_TEMPLATE/bug_report.yml).
- Anything security-shaped — an egress proxy credential reaching a log
  line, a request escaping the free-tier scope:
  [SECURITY.md](SECURITY.md), never a public issue.

## Ownership of what you contribute

You keep the copyright. What you grant is the Apache License 2.0 right to
use and redistribute the work as part of this project — and, per the
section above, a clear statement of which parts a machine helped write.
