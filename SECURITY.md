# Security Policy

## Reporting a vulnerability

**Do not open a public issue.** A public report of a credential leak is
itself a disclosure of the credential.

- Preferred: [a private security advisory](https://github.com/ecoma-io/opencode-free-proxy/security/advisories/new)
  (the repository's _Security_ tab → _Report a vulnerability_).
- Or email **john.itvn@gmail.com** with: a description of the issue, a
  reproduction or proof of concept, and your assessment of the impact.

Please never paste real API keys, live proxy
credentials, or session ids belonging to a real deployment into any report
— a PoC that needs them should hold placeholders.

## What counts as a vulnerability here

opencode-free-proxy sits between client tools and the opencode zen free
tier. Three defect classes therefore count as security
vulnerabilities even when the underlying mechanism is an ordinary bug:

- **A configured credential reaching a place it must not** — an egress
  proxy credential (or any deployment secret) appearing in logs, error
  bodies, or anywhere it was not meant to travel. The upstream credential
  is fixed (`Bearer public`), so the secrets at risk are the ones a
  deployment brings itself.
- **A path that lets a request escape the fixed scope.** The scope is the
  free tier only (`Bearer public`, fingerprint-tool gate, free models) —
  any code path that sends different upstream credentials, reaches a
  non-free upstream model, or relays to a host other than the configured
  upstream base is in scope here.
- **Supply-chain defects in the CI itself** — an unpinned action, an
  unpinned container image, a workflow interpolating attacker-reachable
  input into a shell command. `.github/semgrep/` pins the classes this
  repository treats as vulnerabilities in its own automation.

Everything else — a miscounted usage estimate, a wrong error type, a
session id regenerated a turn early — is an ordinary bug, and the
[public tracker](https://github.com/ecoma-io/opencode-free-proxy/issues)
is the right place for it.

## Supported versions

This project is pre-1.0. Security fixes are applied to the `main` branch
and released in the next version; there is no long-term support branch and
no backport policy for older tags.

## Service levels

Stated honestly for a single-maintainer project:

- **Acknowledgement** within 48 hours of a report.
- **Fix published** within 14 days of a confirmed report — "published"
  meaning a tagged release, not an unmerged commit.
- The **disclosure date** is agreed with the reporter; credit in the
  release notes unless anonymity is preferred.
