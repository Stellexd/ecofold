# Pending Work

This file records the open items found in the review of the
`feat/m1-gpu-provider` branch before it was integrated into `develop`. Each item
was verified as still unresolved at commit `9715fc0`. They must be fixed or
explicitly decided before the `v0.0.1` tag is created on `main`.

Date: 2026-11-09

## Open decision: v0.0.1 scope

`docs/ROADMAP.md` defines Milestone 1 (`v0.0.1`) as "GPU Provider Interface
(NVML + Mock) & FAH Controller". The `internal/fah/` package is empty (it
contains only `.gitkeep`), so the FAH Controller is not implemented.

Options:

- (a) Implement the FAH Controller before tagging `v0.0.1`.
- (b) Move the FAH Controller to Milestone 2 in the roadmap and tag `v0.0.1` as
  "GPU Provider + macOS detector". This is the recommended option because it is
  more accurate and faster.

Status: resolved with option (a). The FAH Controller (`internal/fah`:
`Controller` interface, FAH v8 WebSocket client and `MockController`) was
implemented in branch `feat/m1-fah-controller`. It still needs to be checked
against the real FAH v8.5.6 Docker container on CachyOS before tagging.

## Blockers for v0.0.1

- [x] **A1 (High) Unrecovered panic on binary plist**
  - Where: `internal/detector/darwin_ioreg.go:90` and `:165`
    (`ParseSmartBatteryPlist`, `ParseIOAcceleratorPlist`, and therefore
    `BuildSnapshotFromPlists`).
  - Problem: a 53-byte binary plist causes a panic. `howett.net/plist` v1.0.1
    re-raises `runtime.Error`. Today the input comes from `ioreg` (XML), but a
    panic in the root daemon would kill it before the TDP is restored.
  - Fix: accept only XML and reject the `bplist` magic bytes. Wrap the decode
    in `recover` and convert the panic into an error.

- [x] **A2 (High) Fatal stack overflow on deeply nested XML**
  - Where: `internal/detector/darwin_ioreg.go:16` (`maxPlistBytes`).
  - Problem: an XML document nested deeply enough to stay under the 16 MiB
    limit (about 15 MB) crashes the process. A Go stack overflow is fatal and
    cannot be recovered with `recover`.
  - Fix: scan the nesting depth before decoding and reject anything above a
    limit (for example 64). Optionally lower `maxPlistBytes` to a size that is
    realistic for `ioreg` output.

- [x] **M1 (Medium) Quadratic CPU on binary plist with nesting chains**
  - Where: same as A1.
  - Problem: CPU stays at 100% for minutes on crafted binary input.
  - Fix: resolved by the XML-only change in A1.

- [x] **M2 (Medium) `RestoreDefaultPowerLimit` does not validate the default**
  - Where: `internal/gpu/nvidia_linux.go:136`.
  - Problem: the default limit read from NVML is applied without checking it
    against the device constraints.
  - Fix: read `GetPowerManagementLimitConstraints`, then clamp or reject the
    default before calling `SetPowerManagementLimit`.

- [x] **M5 (Medium) `NewEvaluator` only checks lower bounds**
  - Where: `internal/detector/detector.go:151`.
  - Problem: with `GPUUtilThresholdPct > 100`, folding is never paused for GPU
    load (fail-open).
  - Fix: reset values above 100 to their defaults (also
    `GPUMemRequiresUtilPct`). Add a test for these cases.

- [x] **M7 (Low) `make build-linux` silently builds the non-TDP stub**
  - Where: `Makefile`, target `build-linux` (`GOOS=linux go build ./...`).
  - Problem: on macOS, CGO is disabled, so `internal/gpu/stub_linux.go` is
    compiled instead of the NVML provider. The build passes without any
    power-limit control.
  - Fix: warn or fail when `CGO_ENABLED != 1`, or rename the target so it is
    clearly a compile-only check.

Status: the six blockers above were fixed in branch `fix/m1-review-hardening`.
M5 was extended to also bound `SustainedTicksRequired` (`> 60` resets to the
default), and M2 also rejects a zero maximum constraint. The M2 change was
compiled and tested on Linux with `CGO_ENABLED=1` on 2026-11-11 (see
`docs/HARDENING_REVIEW.md`). The end-to-end NVML test of
`RestoreDefaultPowerLimit` is still pending.

## Residual risks (non-blocking)

Found by the adversarial test pass on `fix/m1-review-hardening`.

- [x] **R1 `FetchIOReg` reads unbounded output** (branch `fix/m1-residual-risks`)
  - Where: `internal/detector/darwin_ioreg.go` (`FetchIOReg`).
  - Problem: `Output()` buffers all of `ioreg`'s stdout before the size check.
  - Fix: read stdout through `io.LimitReader(maxPlistBytes+1)`.
  - Resolution: `FetchIOReg` reads through `cmd.StdoutPipe()` and
    `io.LimitReader`, and kills `ioreg` when the output exceeds the cap.

- [x] **R2 16 MiB plist cap is larger than needed** (branch `fix/m1-residual-risks`)
  - Problem: `howett.net/plist` materializes roughly 10-15x the input in
    memory (about 180 MB worst case). Real `ioreg -d 1` output is under 1 MiB.
  - Fix: lower `maxPlistBytes` (for example to 2 MiB) and adjust the tests.
  - Resolution: `maxPlistBytes` is 2 MiB. The binary plist check now ignores
    leading whitespace. Size-dependent tests and benchmarks were adjusted.

- [ ] **R3 `GPUUtilThresholdPct = 100` plus a huge `GPUMemThresholdMiB`**
  - Problem: both values are in range, but together 99% utilization never
    pauses. Pinned by `TestMaxUtilThresholdPlusHugeMemDisablesPause`.
  - Fix: decide on an upper bound for `GPUMemThresholdMiB` or for the
    utilization threshold.

- [ ] **R4 Transient empty `AppleSmartBattery` output folds at full power**
  - Problem: an empty array is treated as a desktop Mac (`OnAC=true`,
    `BatteryPct=100`). A transient empty result on a laptop would fold as if
    plugged in.
  - Fix: cache `HasBattery` from the first successful read and treat a later
    empty result as an error.

## Deferred to the daemon milestone (M3)

These items will be resolved when the daemon lifecycle and SIGTERM handling
are designed.

- [ ] **M3 NVML shutdown is process-global**
  - Where: `internal/gpu/nvidia_linux.go:19-48`.
  - Problem: two `NvidiaProvider` instances interfere with each other, because
    `nvml.Shutdown` affects the whole process.
  - Fix: add a package-level reference counter or `sync.Once`, or use a single
    provider instance.

- [ ] **M4 Mutex is held during blocking NVML calls**
  - Where: all methods in `internal/gpu/nvidia_linux.go`.
  - Problem: the mutex is held during blocking NVML calls, which could prevent
    restoring the TDP on SIGTERM. This is a hypothesis and has not been
    reproduced.
  - Fix: give the restore path a timeout or bound it, or do not serialize
    restoration behind telemetry reads.

- [x] **M6 MockProvider does not mirror the real lifecycle** (branch `fix/m1-residual-risks`)
  - Where: `internal/gpu/mock_darwin.go`.
  - Problem: the mock works without `Init`, and `Shutdown` is a no-op. NVML
    fails when it is not initialized.
  - Fix: return a "not initialized" error before `Init` and after `Shutdown`,
    matching the Linux stub behavior.
  - Resolution: `MockProvider` and the Linux `stubProvider` track an
    `initialized` flag and return `gpu: provider not initialized` before `Init`
    and after `Shutdown`. Covered by `TestMockProviderLifecycle`.

## Additional hardening

- [x] **`MinBatteryPct > 100` disables battery folding** (branch
  `fix/m1-residual-risks`). An unreachable threshold always paused on battery.
  `NewEvaluator` now resets values above 100 to the default (50). Covered by
  `TestMinBatteryPctAboveHundredIsClamped`.

Items are marked `[x]` when resolved. Reference the commit that fixes each item
in the checklist entry.

See `docs/HARDENING_REVIEW.md` for the review findings and the Linux verification procedure.
