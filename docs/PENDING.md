# Pending Work

This file records the open items found in the review of the
`feat/m1-gpu-provider` branch before it was integrated into `develop`. Each item
was verified as still unresolved at commit `9715fc0`. They must be fixed or
explicitly decided before the `v0.0.1` tag is created on `main`.

Date: 2026-11-09

## Road to v0.1.0 (master checklist)

Added 2026-11-12. This is the ordered plan from the current state (M1 closed,
M2 layer 1 merged) to the `v0.1.0` public release. Tick each box in the PR that
completes it. Items that reference an ID (CH-10, R3, …) are detailed further
down in this file; tick both places.

### Next steps (agreed 2026-11-13, after PR #14)

In this order. Each step is its own branch and PR to `develop`.

1. **Verify the systemd unit on CachyOS** (`deploy/systemd/ecofold-daemon.service`):
   - `SetPowerManagementLimit` works as `User=ecofold` with only
     `CAP_SYS_ADMIN`; otherwise fall back to `User=root` and keep the
     bounding set and sandbox.
   - `ExecStopPost` restores 145 W after `systemctl kill -s KILL`.
   - `/proc/<pid>/comm` and `cmdline` of user games are readable under
     `ProtectSystem=strict`; `DeviceAllow` paths match `ls /dev/nvidia*`.
   - `RuntimeDirectoryPreserve=restart` keeps the pause marker across
     `Restart=`.
   - The in-process cooldown after closing a real game (2 min) ends with
     123 W and FAH folding.
2. **Daemon chaos pass** (new, see the rationale below), branch
   `test/m3-daemon-chaos`, report in `docs/CHAOS_REPORT_M3.md`.
3. **M2 layer 2** (issue #2): Proton/Wine watcher and the Discord/Electron
   filter (section 2 below).
4. **Go 1.27.4** on both machines and in `go.mod` (GO-2026-6859), before any
   IPC socket exists (section 3 below).

Why a chaos pass on the daemon even though M1 is heavily tested: the M1
tests cover the packages in isolation, and the two M1 chaos passes still
found real bugs (CH-1 to CH-11). `internal/daemon` is new, runs as root,
owns the power limit and composes all three packages, and its riskiest
paths are timing and ordering (shutdown during an in-flight NVML or FAH
call, a FAH that flaps or is frozen by `docker pause`, a corrupt or
unwritable marker, decisions that oscillate around the thresholds). Scope it
to the daemon only, do not repeat M1:
- Property tests over random tick sequences (load, battery, Collect errors,
  FAH errors, restarts) checking the invariants: a user pause is never
  resumed; `Resume` never runs while the eco cap is missing on a capable
  GPU; every exit path attempts the restore; no FAH flapping within the
  cooldown.
- `go test -race -count=200` on the `Run`/shutdown tests, and
  cancellation injected at every step of a tick.
- Marker attacks: symlink, directory, read-only or full runtime dir.
- FAH frozen with `docker pause` and killed mid-`Pause` on CachyOS.
Do it after step 1 so the unit's behavior is included, and before the IPC
socket adds a new attack surface (that one gets its own security review).

### 1. Daemon main loop (`cmd/ecofold-daemon`), branch `feat/m3-daemon-loop`

Design and decisions: `docs/DAEMON_PLAN.md`. Implemented in
`internal/daemon` and `cmd/ecofold-daemon`; the macOS items were verified with
`go test -race ./...` on 2026-11-13. Linux items still need a CachyOS run.

- [x] 2 s tick loop: `Collect` → `Evaluator.Evaluate` → apply the power limit
  and `fah.Controller` Pause/Resume (`LinuxCollector` on Linux,
  `DarwinCollector` on macOS).
- [x] Keep the previous decision when `Collect` returns an error (never feed
  "no load" to the evaluator).
- [x] Fail-safe: `signal.NotifyContext(ctx, SIGINT, SIGTERM)` and
  `RestoreDefaultPowerLimit()` on every exit path, including panics in the
  control loop. Restore is bounded (M4) and backed by `ExecStopPost`.
- [x] **CH2-H1:** send `Pause` on game load even when FAH reports itself idle.
- [x] **CH-10:** only `Resume` a pause the daemon made itself, never a manual
  one. Ownership survives restarts through `/run/ecofold/paused-by-daemon`.
- [x] **M2-BLOCK:** `detector.ErrPerProcessUtilUnsupported` latches the daemon
  into `Degraded` (default power limit, FAH left as is) with one clear warning.
- [ ] systemd unit with only `CAP_SYS_ADMIN` (no full root), restart policy and
  `ExecStopPost` that restores the default power limit. Draft in
  `deploy/systemd/ecofold-daemon.service`; verify on CachyOS.
- [x] Pause half of the end-to-end check with *Baldur's Gate 3* on CachyOS
  (2026-11-13): `bg3` at 55-59 % SM was detected after the 3-tick sustain;
  default limit restored 9 ms after the decision and FAH pause confirmed
  1.24 s after it.
- [x] Linux compile and `go test -race ./...` with `CGO_ENABLED=1` on CachyOS
  (2026-11-13). An unprivileged run against the real RTX 5060 and FAH
  container started, collected real per-process data (no M2-BLOCK), detected
  `NVML_ERROR_NO_PERMISSION` and fell back to pause-only without touching FAH.
- [x] End-to-end check on CachyOS: open a game → once the evaluator decides
  to pause (about 6 s of sustained load), TDP back to 145 W and FAH paused
  within 2 s of that decision; resume → 123 W and folding again. Resume half
  verified on 2026-11-13 through the marker path: a restarted daemon read
  `paused-by-daemon`, applied 123 W at once, held `Resume` for the 3 startup
  ticks and resumed project 18264 6.8 s after start; Ctrl+C restored 145 W
  and left FAH folding. The in-process 2-minute cooldown after closing a game
  is covered by unit tests only.

### 2. Close M2 (issue #2): Proton/Wine watcher, branch `feat/m2-proton-watcher`

- [ ] Detection layer 2: `wineserver`, `reaper` and `.exe` payloads as a game
  signal, reading `/proc` once per tick (no shell commands).
- [ ] Share the existing cooldown latch with layer 1.
- [ ] Calibrate the thresholds (45 % / 25 % / 1500 MiB) with real games through
  the daemon and update `internal/detector/README.md`.
- [ ] **R3** and **R4** decided or fixed.
- [ ] Filter Electron helpers: add `discord` (and other Electron binaries) to
  `browserBinaries` in `linux_collector.go`, so `isBrowserHelper` drops their
  `--type=renderer` and `--type=gpu-process` processes. Found on CachyOS:
  Discord's renderer is listed by NVML as a graphics process.

### 3. M3 (issue #3): IPC and GUI

- [ ] Upgrade both machines and `go.mod` to Go 1.27.4 (GO-2026-6859) before any
  socket exists.
- [ ] Unix domain socket with restrictive permissions (owner/group only) and a
  minimal protocol: status, force pause/resume, read/update config.
- [ ] Add Wails to `go.mod`; system tray with state (Folding / Paused by game /
  Battery) and current TDP. The GUI stays unprivileged.
- [ ] GUI handles the daemon being down (shows "daemon not running", no crash).

### 4. M4 (issue #4): CI, packaging and release

- [ ] CI in `.github/workflows`: build, `go test -race`, `go vet` and `gofmt`
  on Linux (CGO) and macOS. Set this up early, ideally during step 1.
- [ ] `govulncheck` in CI.
- [ ] AUR `PKGBUILD`: daemon, systemd unit and GUI.
- [ ] macOS `.dmg` (telemetry + FAH control only; no TDP on Apple Silicon).
- [ ] README with install, permissions and known limitations.
- [ ] Tag `v0.1.0` on `main` once every box above is ticked.

### Housekeeping (any time)

- [x] Disable the legacy `autofold.service` (user unit running
  `~/folding/auto_fold.py`) on CachyOS (2026-11-13). It changed the power
  limit with `sudo nvidia-smi -pl` and froze the FAH container with
  `docker pause`; a frozen container cannot answer the FAH API, so EcoFold
  cannot pause or resume it. The two must never run together.
- [ ] AUR `PKGBUILD`: declare the legacy autofold setup in `conflicts` and
  document in the README that `autofold.service` must be disabled.

- [ ] Publish the FAH container on `127.0.0.1:7396:7396` instead of
  `0.0.0.0:7396` (see "FAH API exposed on the LAN" below).
- [ ] Delete merged branches (`test/m1-chaos*`, `test/m1-nvml-integration`,
  `revert-8-test/m1-nvml-integration`) and the agent worktrees in
  `.claude/worktrees/`.

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
implemented in branch `feat/m1-fah-controller` and verified on 2026-11-11
against the real FAH v8.5.6 Docker container on CachyOS: the WebSocket upgrade
without an `Origin` header returns 101, and a live run went from folding
(project 18264) to paused and back to folding.

- [ ] **FAH API exposed on the LAN.** The `foldingathome` container publishes
  `0.0.0.0:7396`, so any host on the local network can pause or reconfigure
  the client. EcoFold only connects over loopback; publish the port as
  `127.0.0.1:7396:7396` instead. This is a deployment change, not a code change.

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
`docs/HARDENING_REVIEW.md`). `RestoreDefaultPowerLimit` was then verified end
to end on the RTX 5060 with the opt-in `nvml_integration` test (PASS, limit
restored to 145 W).

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

## M2: Linux per-process collector and load cooldown (branch `fix/m1-ch11-nvml-lifecycle`)

- [x] **M2 Linux per-process collector and load cooldown**
  - Where: `internal/detector/linux_collector.go`, `internal/detector/detector.go`,
    `internal/gpu/nvidia_linux.go`, `internal/gpu/provider.go`.
  - Problem: Folding@home keeps the GPU at high device-wide utilization and
    memory use, so aggregate metrics would pause folding permanently. Resuming
    the instant a game releases the GPU also causes oscillation between folding
    and full-power states.
  - Resolution: the Linux collector reads `/proc/<pid>/comm` and `cmdline` to
    filter out the Folding@home core, client, desktop compositors, Steam,
    browser GPU/renderer helpers, and other non-game processes. `gamescope` is
    deliberately kept visible because games can run inside it. The evaluator
    uses per-process SM utilization from NVML when available, ignores global
    utilization when `GPUProcesses` is non-nil, and latches `StatePausedLoad`
    until `CooldownTicksRequired` consecutive load-free ticks elapse (clamped to
    [60, 90] ticks, i.e. two to three minutes at the planned 2 s tick). If the
    battery demands a pause while the load latch is active, `StatePausedBattery`
    is returned instead (no cooldown). Covered by unit tests.

- [ ] **M2-BLOCK per-process GPU utilization unsupported**
  - Where: `internal/detector/linux_collector.go:Collect`.
  - Problem: some NVIDIA drivers/ GPUs return `ERROR_NOT_SUPPORTED` for
    `GetProcessUtilization`, so the collector cannot attribute utilization to
    individual processes and cannot distinguish a game from Folding@home. The
    collector now returns an explicit error when every graphics process has
    `SMUtilValid=false`, so the caller keeps its previous decision instead of
    falsely reporting "no load". EcoFold cannot pause on game load on those
    systems until a fallback (e.g. a process-name heuristic or corrected
    device-wide utilization) is implemented.
  - Fix: implement a fallback load detector for systems without per-process
    utilization, or document this hardware limitation and fail closed at daemon
    startup.

- [x] **M2-INTEGRATION Linux collector read-only integration test**
  - Where: `internal/detector/linux_collector_integration_test.go`.
  - Resolution: opt-in `nvml_integration` test that exercises the real
    collector. It skips when there are no graphics clients and therefore does
    not claim to verify the FahCore filter; that filter is covered by unit
    tests.

## Deferred to the daemon milestone (M3)

These items will be resolved when the daemon lifecycle and SIGTERM handling
are designed.

- [x] **M3 NVML shutdown is process-global** (branch `fix/m1-ch11-nvml-lifecycle`)
  - Where: `internal/gpu/nvidia_linux.go:19-48`.
  - Problem: two `NvidiaProvider` instances interfere with each other, because
    `nvml.Shutdown` affects the whole process.
  - Resolution: `acquireNVML`/`releaseNVML` keep a process-wide reference
    count. `Init` and `Shutdown` are idempotent per provider, a stray
    `Shutdown` before `Init` is a no-op, and only the last `Shutdown` calls
    `nvml.Shutdown`. Covered by `internal/gpu/nvidia_lifecycle_linux_test.go`.

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

## Chaos pass follow-ups (see `docs/CHAOS_REPORT.md`)

- [x] **CH-2 `Close` does not abort an in-flight dial.** Fixed with a client
  context that `Close` cancels. Covered by `TestCloseAbortsInFlightDial`.
- [x] **CH-7 Linux compile check.** `make test` passed on CachyOS with the
  `mwToWCeil` change in `nvidia_linux.go` (2026-11-11).
- [x] **CH2 H1: never skip a pause because FAH looks idle.** A running unit
  with an unreadable project is reported as `idle`. When a game is detected,
  the daemon must send `Pause` regardless of `Status.State`.
- [x] **CH-10 `Resume` unpauses every group.** The daemon must remember whether
  it paused folding itself, and must not resume a pause the user made by hand.
- [x] **CH-11 NVML lifecycle.** Calling `Init` twice leaked an NVML reference,
  and `Shutdown` before `Init` still called `nvml.Shutdown`. Fixed together
  with M3 using a process-wide reference counter plus idempotent per-provider
  `Init`/`Shutdown`.
- [ ] **Go 1.27.4.** govulncheck reports GO-2026-6859 (`net/http` server) in
  Go 1.27.3. Only tests import `net/http` today, but upgrade both machines
  before the daemon gains an HTTP or IPC server.
