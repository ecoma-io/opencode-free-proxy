# Local benchmarks

The benchmark suite is a local development tool. It does not run in CI and it
is not a release gate.

Run the complete suite without ordinary tests:

```sh
go test -run '^$' -bench . -benchmem ./...
```

For a stable, bounded router sample:

```sh
go test -run '^$' -bench BenchmarkRelayHotPath -benchmem -benchtime=200x ./internal/router/
```

To inspect parallel router work at several processor counts:

```sh
go test -run '^$' -bench 'Parallel' -benchmem -cpu=1,2,4 ./internal/router/
```

Compare repeat runs with `benchstat` when investigating elapsed time:

```sh
go install golang.org/x/perf/cmd/benchstat@latest
go test -run '^$' -bench . -benchmem -count=10 ./internal/... > new.txt
benchstat old.txt new.txt
```

## Reading results

- Treat `allocs/op` and `B/op` as the primary, repeatable acceptance metrics.
- Treat `ns/op` as noisy: compare repeated runs (`-count=6` or more) with
  `benchstat`, on the same machine and under comparable load.
- Do not run benchmarks with `-race`; use the normal race-test suite separately.
- Keep benchmark inputs offline and deterministic. Router benchmarks use a
  loopback `httptest` upstream; they never contact the live provider.

## Behavior-preservation rule

A benchmark does not justify a behavior change. Any hot-path optimization must
keep the externally observable response byte-identical, including SSE framing,
response headers, and the fixed completion-log rendering. The router byte-golden
test is the local equivalence check for that contract; run it alongside the
package and end-to-end suites before retaining an optimization.
