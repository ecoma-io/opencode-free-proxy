## Description

Closes #

## Type of change

- [ ] Bug fix — behavior disagrees with the documented contract
- [ ] Contract change — documented behavior moves; README/AGENTS updated in this PR
- [ ] New feature
- [ ] Refactor — no behavior change
- [ ] Documentation only
- [ ] Build / CI / tooling

## Port-parity impact

- [ ] No upstream-shaped behavior changed (session ids, UA gate, fingerprint
      tools, recovery semantics, fail-open contracts)
- [ ] Upstream-shaped behavior changed — the 9router JS citation is in the
      changed comment, and AGENTS.md "Porting discipline" still holds

## Could this fail silently?

<!-- The dangerous direction in this repository is the quiet one: a 429
     moved to another egress when the contract says fail fast, a POST
     replayed after the request may already have reached the provider, a UA
     that silently stays stale past its TTL, a fingerprint tool that stops
     being injected, an SSE line dropped in a relay branch nobody tests.
     Writing "no" is fine when it is true; leaving this blank is not. -->

- [ ] It cannot, and I considered the quiet direction
- [ ] It could, and a test pins the case where it would — test name:

## How this was verified

1.

- [ ] `gofmt -w .` — clean
- [ ] `go vet ./... && go vet -tags e2e ./e2e/` — clean
- [ ] `go test -race ./...` — green
- [ ] `go test -tags e2e ./e2e/` — green (or one line on why E2E is unaffected)

## Checklist

- [ ] Self-reviewed the diff
- [ ] Docs updated in the same pass (README/AGENTS when behavior moves)
- [ ] No API keys, live proxy credentials, or real session ids anywhere in the diff
- [ ] I have the right to contribute this work under the Apache License 2.0

## AI-assisted development

If any commit in this pull request was AI-assisted, the pull request's last
commit carries its disclosure trailer — `Assisted-by: <tool>` or
`Generated-by: <tool>` — one trailer per pull request, not one per commit.

- [ ] No AI-assisted commits in this PR
- [ ] AI-assisted — the disclosure trailer is on the last commit
