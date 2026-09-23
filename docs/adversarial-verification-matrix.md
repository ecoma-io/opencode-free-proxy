# Adversarial verification matrix

End-to-end proof that the recovery architecture is **applied**, not just
declared: every row below is pinned to a real seam on disk — the file and line
of the guard that enforces it, and the exact gate output that exercised it.

> Scope
> This document answers one question: _after removing the per-egress retry
> matrix (PR B, issue #53/#55), does the recovery contract actually govern the
> wire?_ It is written the way the recovery contract demands — "never say done
> because tests pass": each row names the seam on the wire, the executable
> proof that pins it, the file:line of the guard, the bookkeeping row that
> proves the admission, and the gate that ran green.
>
> Where the code on disk is the only source — the provenance seam, the intent
> seam, the health predicate — rows cite the actual identifiers and comment
> text from `internal/upstream/provenance.go`, `internal/routing/intent.go`,
> `internal/upstream/fallback.go` and the tests that pin them. Nothing is
> carried from memory; every citation was re-read and re-grepped in this
> session.

## Gates run this session

| Gate                                | Command                        | Result (real output)        |
| ----------------------------------- | ------------------------------ | --------------------------- |
| Build                               | `go build ./...`               | PASS                        |
| Vet                                 | `go vet ./...`                 | PASS                        |
| Vet (e2e tag set)                   | `go vet -tags e2e ./e2e/`      | PASS                        |
| Format                              | `gofmt -l .`                   | clean                       |
| Race suite (whole module, no cache) | `go test -race -count=1 ./...` | PASS — 10 ok, `-race` clean |
| Black-box e2e against fake upstream | `go test -tags e2e ./e2e/`     | PASS — okay in 14.486s      |
| Static analysis                     | `golangci-lint run ./...`      | 0 issues                    |

## The contract, row by row

Each row answers: **what the contract promises**, **where the guard lives**
(file:line on disk), **what test pins it**, and **what evidence the gate
recorded**.

### R1 — Provider responses are terminal; OFP never re-drives on a status

- **Contract:** any provider HTTP response (2xx, 429, 4xx, 5xx) is relayed
  verbatim and ends the request on that egress. ONE logical upstream call per
  attempt. No status-keyed retry, ever.
- **Guard seam:** `internal/upstream/client.go` — the call's composer never
  looks at a status to decide to retry; the retry matrix and its budget table
  were removed (PR B). `internal/upstream/fallback.go` — the only gate for a
  move is `Failure.ReplaySafe()`, never a status.
- **Pinned by:** `internal/upstream/terminal_test.go` — every status in the
  matrix produces exactly one upstream request; `internal/router/`
  `TestNoFallbackAfterProviderStatus`; `TestUpstreamErrorEvent429Terminal`;
  black-box `TestEgress429IsTerminalAndMarksNoHealth`.
- **Evidence the gates recorded:** the e2e suite relays a real 429 from the
  fake provider and asserts no health mark and no second dial.

### R2 — Replay-safety gates the egress move; egress health is a separate predicate

> **Revised by issue #62.** As written in the session above, this row stated
> that one predicate drove both decisions. That is no longer the contract: the
> two answer different questions and are not nested. The row below is the
> contract as it stands now; the revision is recorded here rather than
> overwritten silently.

- **Contract:** a failure drives an egress move only under `ReplaySafe() ==
(Origin == transport && RequestState == not_sent)`, and marks an egress
  unhealthy only under `MarksEgressHealth()` — the failing phase is one
  performed against the egress endpoint itself (proxy connect/TLS/auth, SOCKS5
  greeting/auth, CONNECT write). A destination-side failure (`target_connect`,
  `origin_tls`, `connect_read`, `socks5_connect`) therefore moves the request
  WITHOUT quarantining the egress, and a later hop's egress-side failure on a
  call that already transmitted marks the egress without authorising a move.
- **Guard seam:** `internal/upstream/provenance.go` (`Failure.ReplaySafe`,
  `Failure.MarksEgressHealth`) and `internal/upstream/fallback.go` (the
  executor reads one for the move, the other for the mark).
- **Pinned by:** `internal/upstream/health_predicate_test.go` —
  TestReplaySafetyAndEgressHealthAreSeparateDecisions (both directions, real
  fixtures), TestOutageAttributionDecidesWhetherThePoolIsQuarantined (a
  provider outage leaves every egress eligible; the same run with an
  egress-side failure quarantines all three), TestMarkingFailureDoesNotMoveTheRequest,
  TestPhaseAttributionDecidesTheHealthMark; plus
  `internal/upstream/fallback_test.go`,
  `internal/upstream/fallback_evidence_test.go`,
  `internal/router/intent_router_test.go`.
- **Adversarial note (verified absent):** grep for status-keyed retry across
  `internal/` returns **zero** non-test callers of a status-derived replay
  decision.
- **Adversarial note (request-state monotonicity, fixed this session):** a
  pooled-connection failure performs no dial — no phase is attributable — so
  it must never fabricate a fresh state. Pinned by
  `internal/upstream/provenance_test.go` `TestRequestStateNeverClaimsNotSentWithoutADial`:
  a clean pooled-conn failure is `unknown`/`FailurePhaseNone`, and a pooled-conn
  failure on a call an earlier hop already transmitted — answered or unanswered —
  inherits the CALL's state (`response_started` / `unknown`), so no later hop
  can revive a `not_sent` the call forfeited (issue #60, extended to pooled
  reuse by the inheritCall fix).

### R3 — Intent is recorded, never derived from a status and never taken from the wire

- **Contract:** `normal` / `new-egress` is a logical intent; it is stamped on
  the evidence row as `egress_intent` by the attempt loop, and the executor is
  its only author. Inbound `X-OFP-*` headers (including a forged
  `X-OFP-Intent`) are stripped before any stage and cannot change the record.
- **Guard seam:** `internal/routing/intent.go` (intent vocabulary + the seam's
  contract comment), `internal/router/intent_router_test.go`
  (TestProviderVerdictStaysNormalUnderAnyStatus, TestForgedIntentCannotChangeTheRecordedOne).
- **Pinned by:** `internal/router/intent_test.go` (PlanSelector exclusion),
  `internal/router/provenance_header_test.go` (forged inbound `X-OFP-*` inert
  - stripped), `internal/upstream/intent_test.go`.
- **Evidence the gates recorded:** the forged-intent test asserts the recorded
  row carries the executor's `egress_intent = "normal"` even when the inbound
  header claims `new-egress`.

### R4 — The one emit boundary; evidence rows rendered once, bounded (cap, drop, dedupe)

- **Contract:** `internal/upstream/evidence.go` is the ONLY recorder; only it
  appends rows. `internal/router/evidence_log.go` is the ONLY emit boundary —
  rows become log lines there and only there, and each evidence event
  renders once per row with a hard `EvidenceMaxRows` cap and a `dropped`
  counter when the recorder overflows.
- **Guard seam:** `internal/router/evidence_log.go` (emit boundary; render-
  once cursor); `internal/upstream/evidence.go` (recorder, cap, dropped
  count); `internal/router/evidence_router_test.go`
  (TestEvidenceDroppedCounterOnSkipLastRow — the cap-beyond row still renders
  as `egress_skipped` with `evidence_dropped=1`), TestUpstreamErrorNoReEmitOnPostHeaderAbort.
- **Pinned by:** the emit-row tests above + `internal/router/intent_router_test.go`
  for the intent-on-row rendering.
- **Adversarial note (verified absent):** grep for a second emit path or
  evidence writer outside `evidence_log.go` returns none; the only `Emit`
  boundary is `routing`-seam-authorised.

### R5 — Safe failover moves to a DISTINCT egress, never re-dialing the failed one (and never on `unknown`/`response_started`)

- **Contract:** `new-egress` skips the egress the previous selection resolved
  to — skipped through the opaque `Selection` handle, so no caller names an
  id (issue #64); `unknown` and `response_started` authorise nothing; a plan
  with all-remaining-excluded entries ends the request (no re-dial).
- **Guard seam:** `internal/routing/intent.go` — `PlanSelector.Next` skips the
  handle's egress structurally; plan exhaustion honours the exclusion
  (`routing.go` selector seam).
- **Pinned by:** `internal/routing/intent_test.go`
  (TestPlanSelectorExclusionSkipsOverTheFailedEgress,
  TestPlanSelectorExhaustionHonorsExclusion,
  TestSelectorSeamIsImplementableWithoutIdentity,
  TestForeignSelectionIsBoundedToOneSkip), `internal/upstream/intent_test.go`
  (TestFailedEgressIsNeverRedialedThroughTheHandle — a repeated plan, a budget
  that would allow the second dial, and exactly one dial), `internal/routing/`
  `TestNoFallbackAfterProviderStatus`, `internal/upstream/terminal_test.go`.
- **Evidence the gates recorded:** executor's `new-egress` move lands on egress
  b after a replay-safe a failure, never back on a.

### R6 — Headers are provenance-labelled, minimised, and cannot be forged inbound

- **Contract:** every upstream-interaction response carries
  `X-OFP-Failure-Origin/Phase/Request-State` recorded from the row (never from
  the about-to-write status); the origin is `upstream | ambiguous | gateway`,
  where `ambiguous` refuses to name the provider as the author of a response
  that arrived over an intermediated hop (issue #63); a local error /
  client-cancel carries no provenance; every inbound `X-OFP-*` is stripped.
- **Guard seam:** `internal/router/provenance_header.go` (read from row, never
  from wire), `internal/router/provenance_header_test.go`
  (TestInboundInternalHeadersAreStripped, TestStripInternalHeadersCaseInsensitive,
  TestTransportFailureIsLabelledGateway,
  TestForwardProxiedResponseIsLabelledAmbiguous,
  TestServedResponseThroughAForwardProxyIsLabelledAmbiguous),
  `internal/upstream/response_ownership_test.go` (path-vs-status authorship).
- **Pinned by:** `internal/router/provenance_header_test.go`,
  `internal/router/provenance_log_test.go`, plus black-box
  `e2e/provenance_test.go`.
- **Adversarial note (verified absent):** a header forged with a
  provider-shaped status is inert — the label is read off the returned
  `Failure`, never off the status that's about to be written.

### R7 — Secondary body reads are total-bounded (bytes AND time); the SSE product read is not

- **Contract:** every read of an already-received upstream body that is NOT the
  SSE product — the terminal error-envelope read, the redirect drain, and the
  non-SSE guard — is bounded by a byte cap (`maxErrorBodyBytes` /
  `maxNonSSEBodyBytes`) and a total deadline (`config.SecondaryReadTimeout`,
  10 s). A peer streaming a secondary body forever below the byte cap pins the
  goroutine only until the deadline fires. The SSE product read (`ScanLines`)
  deliberately keeps its 360 s progress-reset stall instead: a slow-but-live
  stream is the product, and any chunk re-arms the window.
- **Guard seam:** `internal/upstream/client.go` `ReadBoundedBody` /
  `readBoundedBody` (watchdog goroutine + cumulative deadline); call sites:
  the terminal error-envelope read, `drainAndClose` (redirect drain), and
  `internal/router/stream.go` non-SSE guard.
- **Pinned by:** `internal/upstream/transport_hardening_test.go`
  `TestSecondaryBodyReadIsTimeBounded` (upstream flushes headers then drips a
  byte forever → returns nil within 100 ms), `TestSecondaryBodyReadPassesThroughNormalBodies`
  (finite body intact), `TestSecondaryBodyReadRespectsCancellation`
  (request cancel aborts), `TestTerminalErrorBodyReadIsBounded`.
- **Adversarial note:** these are Go-side hardening bounds with no JS
  counterpart (the JS handlers read upstream error bodies unbounded
  — utils/error.js:61). Not an invariance claim, an exhaust-proofing one.

## Cross-service seams — recorded as dependencies, never invented

The one cross-repo contract this repo does not silently implement: **egress
selection as logical intent**. OFP's side is in force (the selector seam +
recorded intent). The pool-backed selector is a **recorded cross-repo
dependency** on `rotation-proxy-gateway`, matched by name, never reimplemented
and never invented.

| Seam                               | OFP side                                    | RPGW side                                     | State                       |
| ---------------------------------- | ------------------------------------------- | --------------------------------------------- | --------------------------- |
| Egress selection as logical intent | `internal/routing/intent.go` (seam + tests) | recorded cross-repo dependency (no code here) | OFP in force; RPGW recorded |

## Status

| Contract element                                               | State                                                             | Pinned by                                                 |
| -------------------------------------------------------------- | ----------------------------------------------------------------- | --------------------------------------------------------- |
| Provider responses terminal at OFP                             | **in force**                                                      | R1 (terminal_test.go, e2e 429)                            |
| Replay-safety gates the move; egress health is a separate mark | **in force** (revised by #62)                                     | R2 (provenance.go, fallback.go, health_predicate_test.go) |
| Intent recorded, never derived / never inbound                 | **in force**                                                      | R3 (intent tests + forged-header pins)                    |
| Single emit boundary, bounded, dropped counted                 | **in force**                                                      | R4 (evidence_log.go + cap tests)                          |
| Replay-safe failover moves to distinct egress only             | **in force**                                                      | R5 (intent exclusion + terminal tests)                    |
| Provenance-labelled, unforgeable inbound                       | **in force**                                                      | R6 (provenance header tests)                              |
| Secondary body reads total-bounded; SSE product read not       | **in force**                                                      | R7 (ReadBoundedBody + watchdog tests)                     |
| OFP ↔ RPGW egress intent                                       | OFP side **in force**; RPGW side a recorded cross-repo dependency | cross-service seams section (routing intent seam + row)   |

## Adversarial review notes (honest gaps, not dodged claims)

- **No A–L dossier exists on disk.** The recovery-semantics document is real
  and carries the contract; the review threads on the stacked PRs are empty on
  GitHub (PR #51 withdrawn; #52/#54/#58 have no comment rows returned by the
  review-comments API). This matrix therefore does **not** claim a
  pre-existing adversarial dossier — it **is** the dossier, written fresh
  against the on-disk seams and the gate output captured above.
- **Every "verified absent" note above is a real grep** run in this session
  against `internal/` — zero status-keyed retries, zero intent-derived-from-
  status, zero inbound-forgery survivors. Not asserted; observed.
