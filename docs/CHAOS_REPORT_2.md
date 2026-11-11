# EcoFold Chaos Report 2: attack on the m1 chaos fixes

## Scope

- Branch under attack: `fix/m1-chaos-findings` at `93cb27e`. Tests are on `test/m1-chaos-2`.
- The diff under attack is `git diff 7130593..93cb27e -- internal/`. It covers the production files
  `internal/fah/client_v8.go` (CH-1, CH-2, CH-3, CH-4, CH-5), `internal/detector/darwin_ioreg.go` (CH-6),
  and `internal/gpu/provider.go` with `nvidia_linux.go` (CH-7).
- Platform: macOS darwin/arm64, Go 1.27.3. No production file was modified.

New test files:

| File | Tag | Purpose |
| --- | --- | --- |
| `internal/fah/chaos2_repro_test.go` | `chaos` | Repros for CH2-1 and CH2-4 (they fail) |
| `internal/fah/chaos2_hardening_test.go` | none | Passing hardening tests and `FuzzChaos2ParseInvariants` |
| `internal/detector/chaos2_repro_darwin_test.go` | `darwin && chaos` | Repro for CH2-2 (it fails) |
| `internal/detector/chaos2_hardening_darwin_test.go` | `darwin` | `statUint` edge values (passes) |
| `internal/gpu/chaos2_repro_test.go` | `chaos` | Repro for CH2-3 (it fails) |
| `internal/gpu/chaos2_hardening_test.go` | none | `mwToWCeil` boundaries (passes) |

## Commands run

| Command | Result |
| --- | --- |
| `go test -race -count=3 ./...` | PASS |
| `go test -tags chaos -race ./internal/... -run Chaos2` | 4 expected FAILs (CH2-1..CH2-4). All other tests pass. |
| `go test ./internal/fah -run '^$' -fuzz FuzzChaos2ParseInvariants -fuzztime 120s` | 20,614,373 execs, 588 corpus entries, no failure |
| `go test ./internal/detector -run '^$' -fuzz FuzzBuildSnapshotFromPlists -fuzztime 60s` | 5,816,868 execs, no panic. This fuzzer does not assert on value signs, so it cannot catch CH2-2. |
| `go test ./internal/fah -count=100 -run '<the 7 regression_test.go tests>\|Chaos2'` | see "Flakiness" below |
| `golangci-lint run ./internal/...` (with and without `--build-tags chaos`) | 0 issues in changed production code. 5 errcheck findings and 1 staticcheck QF1001 are in pre-existing test files. The new chaos2 files are clean. |
| `make test` | PASS |

### Flakiness

The 7 tests in `internal/fah/regression_test.go` and the untagged `Chaos2` fah tests passed 100 out of 100 runs (`-count=100`, 186.9 s, 0 failures). No flakiness was observed.

## Findings (ranked by severity)

| ID | Severity | Title | Location | Status | Failure scenario | Repro | Suggested fix |
| --- | --- | --- | --- | --- | --- | --- | --- |
| CH2-2 | Low | A huge `<integer>` performance statistic wraps to a negative utilization | `internal/detector/darwin_ioreg.go:192-195`, `statUint` at `:209` | CONFIRMED | howett.net/plist decodes an `<integer>` above MaxInt64 as `uint64`. `statUint` passes it through, and `int(...)` wraps it: 2^64-1 becomes `GPUDeviceUtilPct=-1`, and 2^63 becomes MinInt64. A negative utilization can never reach a threshold, so a corrupt or hostile ioreg value hides GPU load (fail-open for detection). | `TestChaos2HugeIntegerStatWrapsNegative`: `go test -tags chaos ./internal/detector -run TestChaos2HugeIntegerStatWrapsNegative` | Clamp in `statUint` (for example `min(v, math.MaxInt32)`), or clamp the percentages to [0,100] after conversion. |
| CH2-3 | Low | The ceiled minimum can exceed the floored maximum | `internal/gpu/provider.go:79` (`mwToWCeil`), `internal/gpu/nvidia_linux.go:99-100` | CONFIRMED (pure function). The impact on callers is a HYPOTHESIS. | For an NVML range such as [123500, 123900] mW, the advertised range is `MinPowerLimitW=124 > MaxPowerLimitW=123`. Any whole-watt setting in that range is out of bounds. A caller that clamps `max(min, min(x, max))` would request 124 W, which NVML rejects. No production caller reads `MinPowerLimitW` yet. | `TestChaos2CeilMinExceedsFlooredMax`: `go test -tags chaos ./internal/gpu -run TestChaos2CeilMinExceedsFlooredMax` | Document that Min > Max means no settable whole-watt limit, and make the future clamp logic treat this as "unsupported" instead of setting a value. Alternatively, detect it in `GetTelemetry` and report it. |
| CH2-4 | Low | One unconfirmed Pause opens about 60 TCP connections | `internal/fah/client_v8.go:255-276` (`confirmPaused`) | CONFIRMED (measurement) | Take a FAH client that accepts the command but never reports paused (for example a group that cannot pause, or a schema change in `paused`). One `Pause` with the default 3 s timeout runs 60 full TCP and WebSocket handshakes, each followed by a JSON snapshot parse, measured against a loopback fake. If the daemon retries every tick, the result is steady connection churn and TIME_WAIT sockets against FAH. | `TestChaos2PauseConnectionAmplification`: `go test -tags chaos ./internal/fah -run TestChaos2PauseConnectionAmplification -v` (logs `connections=60`) | Confirm over the command's own connection by reading the incremental `["groups",...,"paused",true]` updates, or back off exponentially (50, 100, 200 ms and so on) between polls. |
| CH2-1 | Info | `Close` on a `ClientV8` not built by `NewClientV8` panics | `internal/fah/client_v8.go:208` (`c.shutdown()`), and also `c.dial` and `c.done` in `do` | CONFIRMED | Before the fix, `Close` on a zero-value client was a no-op. It now dereferences a nil `shutdown` func. `do` would also panic on a nil `dial` or `done`. The zero value was already unusable for calls (nil `conns` map), so this only affects `defer c.Close()` patterns and tests. | `TestChaos2ZeroValueClosePanics`: `go test -tags chaos ./internal/fah -run TestChaos2ZeroValueClosePanics` | Guard `if c.shutdown != nil`, or document that `ClientV8` must be created with `NewClientV8`. |

### Hypotheses (no failing test)

- **CH2-H1 (Info): a running unit with an unparseable project is reported as idle.** The whole unit is skipped when any field has an unexpected type: `"project":"0x48C2"`, `" 18626"`, `"+18626"`, an object, or `pause_reason` as a number. In that case `parseFAHMessage` reports `StateIdle` while FAH is really folding. This is the documented fail-closed choice for `paused`. For the folding state it is fail-open only if the daemon skips the pause when the state is idle. That depends on daemon code that does not exist yet. `TestChaos2ProjectIDEdges` pins the current mapping.
- **CH2-H2 (Info): confirmation trusts any snapshot.** If FAH was already paused, `Pause` is confirmed as soon as the first poll succeeds. The command was delivered anyway, because the write succeeded on a live connection, so this is harmless. A peer that lies about `paused` cannot be detected by any client-side design.
- **CH2-H3 (Info): raw non-ASCII in the query passes CH-4.** `ws://127.0.0.1/api?a= ` passes, and UTF-8 bytes `C2 A0` go onto the request line. These bytes contain no CR, LF or space, so the request line cannot be split. FAH would just see an odd query. This is not a vulnerability.

## Attacked and held up

- **CH-1:** errors from an unconfirmed `Pause` wrap `context.DeadlineExceeded`, or `context.Canceled` (and not DeadlineExceeded) on a cancel. `Close` during confirmation yields `ErrClosed` (the existing test, run 100 times). Timers are stopped on ctx exit. No goroutine leak.
- **CH-2:** 500 sequential calls followed by `waitForGoroutines` show no leak from `AfterFunc(c.done, …)`. `Close` twice is idempotent. `GetStatus` and `Pause` after `Close` return `ErrClosed`. 20 concurrent calls racing `Close` end with nil or `ErrClosed`, without panics under `-race`.
- **CH-5:** a refused dial is not reported as DeadlineExceeded or ErrClosed. Precedence is ErrClosed, then ctx.Err(), then a net timeout at or after the deadline. A non-timeout error such as an RST is never upgraded to DeadlineExceeded unless ctx has already expired.
- **CH-3:** the invariants `Paused ⇔ StatePaused` and `ActiveProjectID>0 ⇔ StateFolding` held, `ActiveProjectID` was never negative, and errors always came with `StateUnknown`, over 20.6M fuzz execs. Numeric edges map as expected: 1e5 → 100000, 1E4 → 10000, 18626.0000000001 → 0, -0 → 0, 2147483647 is accepted, 2147483648 → 0, 1e400 → 0, and 30-digit integers → 0.
- **CH-4:** space, tab and DEL in the query are rejected. Percent-encoded `%0A` and `%20` stay encoded and are accepted, which is correct. A 4 KiB query, `[::1]` and `LOCALHOST` are accepted.
- **CH-6:** `statUint` maps NaN, ±Inf, -0, 2^63, MinInt64, negatives, nil, maps, arrays, bool and string to 0. 2^63-1024 converts exactly. `FuzzBuildSnapshotFromPlists` ran 5.8M execs without a panic.
- **CH-7:** `mwToWCeil` is correct at 0, 1, 999, 1000, 1001 and MaxUint32 (4294968, no overflow thanks to the uint64 widening), and it is always floor or floor+1.

## Not tested

- The Linux NVML path (`nvidia_linux.go`) on real hardware. Only the pure conversion was exercised on macOS.
- Server RST that arrives exactly at the deadline (a timing race that cannot be produced deterministically on loopback).
- Daemon-level consequences of CH2-1, CH2-3 and CH2-H1: the daemon control loop is not implemented yet.
- govulncheck was not re-run, because the dependency set did not change in the diff.
