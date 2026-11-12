# Chaos Report: Milestone 1 (`internal/fah`, `internal/detector`, `internal/gpu`)

Date: 2026-11-11

## 1. Scope

| Item | Value |
| --- | --- |
| Commit tested | `1355e7a` (`develop`, "Merge pull request #10 from Stellexd/docs/m1-nvml-e2e-verified") |
| Branch | `test/m1-chaos` (local only, not pushed) |
| Platform | macOS Apple Silicon (`darwin/arm64`), Go 1.27.3 |
| Packages | `internal/fah` (`client_v8.go`, `mock.go`, `controller.go`), `internal/detector` (`detector.go`, `darwin_ioreg.go`), `internal/gpu` (`provider.go`, `mock_darwin.go`; `nvidia_linux.go` by reading only) |
| Rules | No production file was changed. Failing repros are behind the `chaos` build tag, so `make test` stays green. Only loopback sockets were used; nothing connected to port 7396. |

`nvidia_linux.go` needs `linux && cgo` and NVML, so it was not compiled or
run here. Every finding about it is marked HYPOTHESIS.

## 2. Tools and commands

| Tool / command | Result |
| --- | --- |
| `golangci-lint run ./...` (v2.15.0) | 5 issues, all in test files: 4 `errcheck` on `defer x.Close()`, 1 staticcheck QF1001 (De Morgan). Style only. |
| `golangci-lint run --enable gosec,errcheck,staticcheck,bodyclose,noctx,gocritic ./...` | 18 issues. Triage: G115 uint64→int in `darwin_ioreg.go` (already H2 in `SECURITY_AUDIT.md`); G505/G401 SHA-1 is required by RFC 6455 for `Sec-WebSocket-Accept` (false positive); G204 `ioreg` class argument is a constant at every call site (false positive); G304 / noctx in tests only. No new real bug. |
| `GOOS=linux CGO_ENABLED=1 golangci-lint run ./internal/gpu/` | Not possible on macOS (cgo preprocessing of go-nvml fails). |
| `govulncheck ./...` | 0 vulnerabilities reachable. 1 in the standard library not called by EcoFold: GO-2026-6859 (`net/http` server, `MaxBytesReader` header race), fixed in Go 1.27.4. `net/http` is only imported by tests. |
| `GOOS=linux CGO_ENABLED=0 go vet ./...`, `go vet ./...`, `go vet -tags chaos ./...` | Clean. |
| Native fuzzing (`go test -fuzz`) | See the table below. No crasher, no invariant violation. |
| Races | `go test -race` on every new concurrency test, plus `-count=300` on the existing timeout test (see CH-5). |
| `make test` on `test/m1-chaos` | Green (detector, fah, gpu `ok`). Because of CH-5, the existing `TestClientV8DefaultTimeout` can still fail at random; it failed once in the `go test -tags chaos -race ./...` run. |
| `go test -tags chaos -race ./...` | Fails as expected: CH-1, CH-3, CH-4, CH-5, CH-6 and CH-7 repros fail; CH-2 skips on macOS. |

Fuzzing (`go test -run '^$' -fuzz <name> -fuzztime <t> ./internal/<pkg>`):

| Fuzzer | Package | Time | Execs | Result |
| --- | --- | --- | --- | --- |
| `FuzzReadMessage` (new) | fah | 2m | 9.40M | PASS |
| `FuzzReadHandshakeResponse` (new) | fah | 2m | 9.16M | PASS |
| `FuzzValidateEndpoint` (new) | fah | 2m | 11.18M | PASS |
| `FuzzParseFAHMessage` (existing) | fah | 1m | 5.17M | PASS |
| `FuzzNewEvaluatorConfigSpace` (new) | detector | 1m | 21.84M | PASS |
| `FuzzValidateThenDecodeStaysXML` (existing) | detector | 1m | 0.97M | PASS |
| `FuzzBuildSnapshotFromPlists` (existing) | detector | 1m | 5.34M | PASS |
| `FuzzPowerConversions` (new) | gpu | 1m | 24.77M | PASS |

New fuzzers check invariants, not only "no panic": `FuzzReadMessage` (message
never above 1 MiB or above the input size), `FuzzReadHandshakeResponse`
(accepted handshakes are at most 16 KiB, are HTTP/1.x and contain the accept
key and the upgrade), `FuzzValidateEndpoint` (every accepted endpoint dials
exactly `127.0.0.1` or `::1` on a numeric port, and the Host header and
request URI contain no CR, LF or NUL), `FuzzNewEvaluatorConfigSpace` (clamp
invariants hold for any `Config`, 100% GPU always pauses within
`SustainedTicksRequired` ticks, an idle machine on AC never pauses) and
`FuzzPowerConversions` (W↔mW round trip, floor semantics, clamp bounds,
MiB conversion).

## 3. Findings

Severity is for the current code and the planned daemon use. Nothing here is
Critical or High: the FAH endpoint is loopback-only and the plist input comes
from the kernel through `/usr/sbin/ioreg`.

| ID | Severity | Title | Location | Status |
| --- | --- | --- | --- | --- |
| CH-1 | Medium | `Pause`/`Resume` report success without any delivery or acknowledgement | `internal/fah/client_v8.go:199-217`, `:254` | CONFIRMED |
| CH-5 | Low | Deadline errors are sometimes not wrapped as `context.DeadlineExceeded`; the existing `TestClientV8DefaultTimeout` is flaky | `internal/fah/client_v8.go:258-266` | CONFIRMED |
| CH-3 | Low | `Pause` is blocked by any type drift in fields it does not need (units) | `internal/fah/client_v8.go:574`, `:559` | CONFIRMED |
| CH-6 | Low | One non-integer entry in `PerformanceStatistics` discards all GPU telemetry | `internal/detector/darwin_ioreg.go:94` | CONFIRMED (real-world drift is a HYPOTHESIS) |
| CH-7 | Low | Advertised `MinPowerLimitW` is not settable when the NVML minimum is not whole watts | `internal/gpu/nvidia_linux.go:99`, `:121-127`; `internal/gpu/provider.go:72` | CONFIRMED at helper level, HYPOTHESIS end to end |
| CH-2 | Low | `Close()` does not abort a call that is still dialing | `internal/fah/client_v8.go:179-189`, `:232` | HYPOTHESIS (Linux); cannot be triggered on macOS |
| CH-4 | Info | A space in the endpoint query is copied into the HTTP request line | `internal/fah/client_v8.go:145`, `:303` | CONFIRMED |
| CH-8 | Info | Allocation amplification: about 7 MiB allocated per 1 MiB fragmented message | `internal/fah/client_v8.go:465` | CONFIRMED (bounded) |
| CH-9 | Info | A cancel between the close frame and the drain deadline is ignored for up to 250 ms | `internal/fah/client_v8.go:522-531` | HYPOTHESIS (window of microseconds) |
| CH-10 | Info | `Resume` unpauses every group, including groups the user paused by hand | `internal/fah/client_v8.go:158-160`, `:190-197` | Design note (daemon not written yet) |
| CH-11 | Info | NVML lifecycle parity: `Init` twice leaks an NVML reference; `Shutdown` before `Init` still calls `nvml.Shutdown` | `internal/gpu/nvidia_linux.go:18-47` | HYPOTHESIS (new angle on M3) |

### CH-1 (Medium): `Pause`/`Resume` report success without delivery

- **Scenario:** the FAH client sends its snapshot and then crashes, restarts
  or closes the socket (FAH update, container restart, OOM). `sendState`
  writes the command into the local socket buffer, which succeeds; the error
  of the following close handshake is swallowed by `closeGracefully`, so
  `Pause` returns `nil`. The daemon then believes folding is paused while the
  GPU keeps folding during the game (or, for `Resume`, believes folding
  resumed). Nothing in the API lets the caller tell the difference.
- **Repro:** `TestChaosPauseReportsSuccessWithoutDelivery` (server reads
  nothing after the snapshot and closes). Fails deterministically.
  `go test -tags chaos -race -run TestChaosPauseReportsSuccessWithoutDelivery -v ./internal/fah`
- **Fix:** after sending the command, read until the FAH incremental update
  that shows the new `paused` value (or open a second call and verify with
  `GetStatus`), and return an error if it does not arrive before the deadline.

### CH-5 (Low): deadline errors not always classified as `DeadlineExceeded`

- **Scenario:** the conn deadline and the ctx timer are set to the same
  instant. When the netpoller fires first, the read fails with a bare
  `i/o timeout` while `ctx.Err()` is still `nil`, so `do()` takes the default
  branch and the error does not wrap `context.DeadlineExceeded`. A daemon
  that uses `errors.Is(err, context.DeadlineExceeded)` to decide between
  "FAH is slow" and "FAH is broken" gets a random answer. The existing test
  `TestClientV8DefaultTimeout` asserts the wrapped error and fails 38/300
  runs with `-race` and 55/300 without, so `make test` is flaky today.
- **Repro:** `TestChaosDeadlineErrorClassificationRace` (20-30 of 40 calls
  misclassified per run).
  `go test -tags chaos -race -run TestChaosDeadlineErrorClassificationRace -v ./internal/fah`
  and `go test -race -run TestClientV8DefaultTimeout -count=300 ./internal/fah`
- **Fix:** in `do()`, also treat `errors.Is(err, os.ErrDeadlineExceeded)`
  with `!time.Now().Before(deadline)` as `context.DeadlineExceeded`, or wait
  for `<-ctx.Done()` briefly before classifying.

### CH-3 (Low): `Pause` blocked by unit schema drift (fail-open)

- **Scenario:** `sendState` only sends the command after `readStatus`
  decodes the full snapshot into typed structs. If any unit has a field of an
  unexpected type (`"project": 18626.0`, `"project": "18626"`, or
  `"assignment": []` on one unit), `json.Unmarshal` fails, `Pause` returns an
  error and the command is never sent. Folding is never paused while that
  unit stays in the snapshot. Pausing does not need the unit fields at all.
  Real FAH v8.5.6 emits integers, so this needs a FAH change or a corrupt
  unit to trigger.
- **Repro:** `TestChaosPauseBlockedByUnitSchemaDrift` (3 sub-cases).
  `go test -tags chaos -race -run TestChaosPauseBlockedByUnitSchemaDrift -v ./internal/fah`
- **Fix:** for `Pause`/`Resume`, only require that the first message is a
  JSON object with `config` or `groups`; decode units leniently (for example
  `json.Number` or `json.RawMessage` per unit, skipping units that fail).

### CH-6 (Low): one foreign statistic type discards GPU telemetry

- **Scenario:** `PerformanceStatistics` is decoded as `map[string]uint64`.
  A single string, real, boolean or array entry, in a key EcoFold never
  reads, makes `ParseIOAcceleratorPlist` fail, so the game-load layer gets no
  GPU data at all. The M4 fixture only has integers, but other Apple GPUs or
  a macOS update can add other types (HYPOTHESIS). The same strictness
  applies to the typed fields of `ioregBatteryNode` (for example a `<real>`
  `Voltage`).
- **Repro:** `TestChaosForeignPerformanceStatisticTypeBreaksGPUParse`.
  `go test -tags chaos -race -run TestChaosForeignPerformanceStatisticTypeBreaksGPUParse -v ./internal/detector`
- **Fix:** decode `PerformanceStatistics` as `map[string]any` and read only
  the four keys with a type switch (integer → value, anything else → 0).

### CH-7 (Low): advertised minimum power limit is not settable

- **Scenario:** `GetTelemetry` reports `MinPowerLimitW = mwToW(minLimit)`
  (floor). `SetPowerLimit(w)` converts back with `wToMW` and rejects
  `mw < minLimit`. If NVML reports a minimum such as 123500 mW, telemetry
  says 123 W but `SetPowerLimit(123)` fails, so a daemon or GUI that uses the
  advertised minimum as the eco limit can never apply it. The RTX 5060 used
  in development reports exactly 123000 mW, so it is not affected.
- **Repro:** `TestChaosAdvertisedMinPowerLimitIsNotSettable` replays the
  exact conversions and range check of `nvidia_linux.go`.
  `go test -tags chaos -race -run TestChaosAdvertisedMinPowerLimitIsNotSettable -v ./internal/gpu`
- **Fix:** round the minimum up (`(mw+999)/1000`) when reporting it, or clamp
  the requested value to the constraints in `SetPowerLimit` instead of
  rejecting values within 1 W of the minimum.

### CH-2 (Low, HYPOTHESIS): `Close()` does not abort an in-flight dial

- **Scenario:** `Close()` only closes connections already in `c.conns`. A
  call that is still in `DialContext` keeps waiting until its ctx deadline
  (3 s by default). On Linux, a full accept queue on port 7396 drops SYNs
  (`tcp_abort_on_overflow=0`), so the dial hangs and shutdown is delayed.
  macOS answers a full queue with RST, so the window cannot be held open
  here.
- **Repro:** `TestChaosCloseDoesNotAbortInFlightDial` (skips on macOS,
  expected to fail on Linux).
  `go test -tags chaos -race -run TestChaosCloseDoesNotAbortInFlightDial -v ./internal/fah`
- **Fix:** give each call a ctx derived from a client-level ctx that
  `Close()` cancels, so the dial is aborted too.

### CH-4 (Info): space in the endpoint query reaches the request line

- **Scenario:** `net/url` accepts a raw space in the query, and
  `u.RequestURI()` keeps it, so `ws://127.0.0.1:7396/api/websocket?x HTTP/1.0`
  produces `GET /api/websocket?x HTTP/1.0 HTTP/1.1`. CR/LF are rejected by
  `net/url`, so no header injection is possible, and the endpoint is local
  configuration. Related: `ws://127.0.0.1?x=1` silently drops the query
  because `u.Path` is empty.
- **Repro:** `TestChaosQuerySpaceBreaksRequestLine`.
  `go test -tags chaos -race -run TestChaosQuerySpaceBreaksRequestLine -v ./internal/fah`
- **Fix:** reject endpoints with a query (FAH does not need one), or reject
  any byte `<= 0x20` or `>= 0x7F` in `RequestURI()`.

### CH-8 (Info): allocation amplification on fragmented messages

- **Scenario:** `append(msg, make([]byte, n)...)` grows the slice by about
  1.25x per step once it is large. A hostile local server that sends 4
  skipped 1 MiB messages in 1023 fragments each makes one `GetStatus`
  allocate about 28.6 MiB (peak live memory stays about 2 MiB). Bounded, so
  not a DoS, but avoidable GC churn.
- **Test:** `TestChaosWorstCaseAllocationIsBounded` (passes, logs the
  number, fails above 32 MiB).
- **Fix:** keep the per-message buffer and grow it with `slices.Grow` to
  `len(msg)+n` only when needed, or reject non-final fragments smaller than a
  minimum size.

### CH-9 (Info, HYPOTHESIS): cancel ignored for up to 250 ms during the close drain

`closeGracefully` calls `SetReadDeadline(drainUntil)`. If the ctx is
canceled after the close frame is written but before this call, the
`AfterFunc` deadline set to "now" is overwritten and the drain waits up to
`closeDrainTimeout` (250 ms). The window is a few microseconds, so it was not
reproduced. Fix: check `ctx.Err()` after setting the read deadline.

### CH-10 (Info): `Resume` unpauses all groups

`Resume` sends `{"cmd":"state","state":"fold"}` without `group`, which
resumes every resource group. If the user paused FAH (or one group) before
the game started, the daemon's `Resume` after the game overrides that
choice. This is documented behavior of the client, but the daemon (M3) must
remember the pre-pause state and only resume what it paused.

### CH-11 (Info, HYPOTHESIS): NVML lifecycle parity

Reading `nvidia_linux.go`: `Init` has no "already initialized" guard, so a
second `Init` calls `nvml.Init` again (NVML reference count 2) and a single
`Shutdown` leaves NVML loaded. `Shutdown` before `Init` (or twice) still calls
`nvml.Shutdown`, which can drop a reference held by another user of NVML in
the process. The mock and Linux stub return `nil` in these cases, so tests on
macOS cannot catch it. This extends M3; fix it together with M3 (package
level reference counting or `sync.Once`).

## 4. Attacked and held up

| Area | What was tried | Result |
| --- | --- | --- |
| Endpoint policy | Fuzzing (`FuzzValidateEndpoint`) plus explicit cases: decimal/octal/hex/short IPv4 (`2130706433`, `0x7f.1`, `017700000001`, `0177.0.0.1`, `127.1`), `localhost.`, `[::1%25lo0]`, `[0:0:0:0:0:0:0:1]`, IPv4-mapped IPv6, `@` and `#@` tricks, backslash, full-width `ｌｏｃａｌｈｏｓｔ`, percent-encoded host, spaces, `:+80`, `:0x50`, `:-1`, 64 KiB host | All rejected or dial exactly `127.0.0.1`/`::1`. `localhost` is pinned to `127.0.0.1` and never resolved. No CR/LF/NUL reaches the request. |
| Handshake parser | Fuzzing (`FuzzReadHandshakeResponse`); exact header-count boundary | 64 headers accepted, 65 rejected (the guard counts the blank line, so it is not off by one); 16 KiB / 4 KiB limits hold. |
| Frame reader | Fuzzing (`FuzzReadMessage`); frame of exactly 1 MiB accepted and 1 MiB + 1 rejected before allocation; pings and pongs interleaved between every fragment of the snapshot | Cap and frame count hold; every ping answered; message intact. |
| Slowloris | One byte every 20 ms in the handshake, in a frame payload, and as a ping drip, with a 300 ms deadline | Every call returns within about 340 ms (only the error class is wrong, see CH-5). |
| Dead contexts | Already-expired deadline and already-canceled ctx on `Pause` | Fail in under 50 ms without opening any connection. |
| Close races | 16 goroutines × 5 calls racing `Close()` (twice), 5 rounds, `-race` | No race, no panic, every call within its deadline, `conns` map empty afterwards, no goroutine leak. |
| Parser | `FuzzParseFAHMessage` (existing) with the new seeds; multiple groups, empty group names, negative/zero project, duplicate keys | Status invariants hold (`Paused` ⇔ `StatePaused`, project ⇔ `StateFolding`). |
| Evaluator | `FuzzNewEvaluatorConfigSpace` over the full `Config` space | All clamp invariants hold; 100% GPU always pauses; idle on AC never pauses. |
| Plist | Existing `FuzzValidateThenDecodeStaysXML` and `FuzzBuildSnapshotFromPlists` re-run; integer overflow (2^64), hex integers, duplicate keys, negative values | No panic; out-of-range integers are rejected by the decoder; negative or wrapped values are the known H2 item. |
| GPU helpers | `FuzzPowerConversions`; 32-bit `int` overflow in `wToMW` reviewed | Round trip exact for every accepted watt value; on 32-bit platforms the wrap-around still yields the right `uint32`. |
| Mock lifecycle | 8 goroutines racing `Init`/`Shutdown` with every getter and setter, `-race` | No race, no torn telemetry, `SetPowerLimit`/`RestoreDefaultPowerLimit` never succeed. |
| go-nvml process list | Reviewed `DeviceGetGraphicsRunningProcesses` in go-nvml v0.13.4-1 | It retries on `ERROR_INSUFFICIENT_SIZE`, so a game starting between calls does not cause a spurious error. |

## 5. Not tested

- `nvidia_linux.go` on real hardware or under the race detector (needs
  Linux, cgo and NVML). CH-2, CH-7 (end to end) and CH-11 should be run on the
  CachyOS workstation: `go test -tags chaos -race ./internal/fah ./internal/gpu`.
- The daemon and GUI: `cmd/ecofold-daemon` and `cmd/ecofold-gui` are empty, so
  the fail-safe TDP restore path, IPC and signal handling could not be
  attacked.
- `FetchIOReg` against a real hung or oversized `ioreg` (it is a fixed system
  binary; the size guard is covered by existing tests).

## 6. Files added

| File | Tag | Purpose |
| --- | --- | --- |
| `internal/fah/chaos_repro_test.go` | `chaos` | CH-1, CH-2, CH-3, CH-4, CH-5 repros (fail) |
| `internal/fah/chaos_hardening_test.go` | none | Fuzzers and passing hardening tests |
| `internal/fah/chaos_helpers_test.go` | none | `acceptFor` helper |
| `internal/detector/chaos_darwin_repro_test.go` | `darwin && chaos` | CH-6 repro (fails) |
| `internal/detector/chaos_hardening_test.go` | none | `FuzzNewEvaluatorConfigSpace` |
| `internal/gpu/chaos_repro_test.go` | `chaos` | CH-7 repro (fails) |
| `internal/gpu/chaos_hardening_test.go` | none | `FuzzPowerConversions` |
| `internal/gpu/chaos_mock_darwin_test.go` | `darwin` | Mock lifecycle race test |

Run all repros: `go test -tags chaos -race -run Chaos -v ./internal/...`

## 7. Resolution (branch `fix/m1-chaos-findings`)

| ID | Status | Fix | Regression test |
| --- | --- | --- | --- |
| CH-1 | Fixed | `Pause`/`Resume` return nil only after a fresh snapshot confirms the requested `Paused` value (polled every 50 ms within the ctx deadline). `Close` stops the confirmation with `ErrClosed`. | `TestPauseFailsWithoutDelivery`, `TestCloseStopsPauseConfirmation` |
| CH-5 | Fixed | `wrapErr` reports a net timeout at or after the ctx deadline as `context.DeadlineExceeded`, for dial and I/O errors alike. `TestClientV8DefaultTimeout` passed 300/300 runs. | `TestDeadlineErrorAlwaysWrapsDeadlineExceeded` |
| CH-3 | Fixed | Units are decoded one by one and invalid units are skipped. The project is read as `json.Number`, so integral floats and numeric strings are accepted, and anything else maps to 0. `paused` is still strict, so the parser fails closed. | `TestPauseToleratesUnitSchemaDrift`, `TestParseFAHMessageToleratesUnitSchemaDrift` |
| CH-4 | Fixed | `validateEndpoint` rejects request URIs that contain bytes `<= 0x20` or `0x7F`. | `TestNewClientV8RejectsSpacesInRequestURI` |
| CH-6 | Fixed | `PerformanceStatistics` is decoded as `map[string]any`. `statUint` accepts integers and finite non-negative reals; any other value maps to 0. | `TestForeignPerformanceStatisticTypeKeepsGPUParse`, `TestPerformanceStatisticTypesDegrade` |
| CH-7 | Fixed | `Telemetry.MinPowerLimitW` uses `mwToWCeil`. `make test` passed on CachyOS with `CGO_ENABLED=1`. | `TestAdvertisedMinPowerLimitIsSettable`, `TestMwToWCeil` |
| CH-2 | Fixed | Every call derives its context from a client context that `Close` cancels, so a dial still in progress is aborted. The original repro also skipped on Linux: 1024 filler connections never fill a `somaxconn=4096` backlog. It was replaced by a deterministic test that uses a blocking dialer. | `TestCloseAbortsInFlightDial` |
| CH-8, CH-9 | Accepted | Bounded behavior; no change. | none |
| CH-10, CH-11 | Deferred | Daemon milestone (see `docs/PENDING.md`). | none |

Side effect of CH-1: within one process, concurrent Pause and Resume calls now
contradict each other until a deadline, because each one waits for its own
state. `TestClientV8Concurrent` now runs a pause phase and then a resume phase.
The daemon must issue one command at a time.
