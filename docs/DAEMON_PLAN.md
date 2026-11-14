# Daemon Main Loop Plan (`feat/m3-daemon-loop`)

Date: 2026-11-13. Base: `develop` at `deebb3c`. Status: **approved and
implemented** (macOS verified; CachyOS verification pending).

Decisions taken on 2026-11-13: F2 reword the E2E check; F3 periodic `ioreg`
allowed (skill amended); F7 power first in both directions, and `Resume` only
after the eco cap succeeds (or power limits are unsupported); §3.6 never
resume FAH on exit, keep the marker. Added during implementation:
`StartupHoldTicks` (a pause inherited through the marker is not resumed until
the evaluator has seen `SustainedTicksRequired` snapshots), startup restore of
the default limit (an unknown limit left by a crash is normalized), and
`gpu.ErrNoPermission` so NO_PERMISSION can be detected with `errors.Is`.

This plan covers step 1 of "Road to v0.1.0" in `docs/PENDING.md`. The IPC
socket and GUI (step 3) are out of scope.

Target for today: everything compiles and passes `go test -race ./...` on
macOS (`darwin/arm64`) with `MockProvider`, the darwin ioreg collector,
`MockController` and test fakes. Linux/NVML/systemd verification on CachyOS is
documented here and deferred.

---

## 1. Findings that change or refine the agreed design

Each item lists what the code does today and what the plan proposes. Items
marked **DECIDE** need an answer before implementation.

### F1. `StateFolding` vs `StateFoldingEco` on Linux

`detector.go` documents `StateFolding` as "normal power limit (100% TDP)" and
`StateFoldingEco` as "on Linux, the daemon applies the configured watt limit".
But `LinuxCollector` always reports `HasBattery=false, OnAC=true`, so on Linux
the evaluator **never** returns `StateFoldingEco`.

Plan (matches the agreed definition): on Linux with `SupportsPowerLimit`, both
`StateFolding` and `StateFoldingEco` apply the eco cap (123 W). Fix the
misleading comments in `detector.go` in the same PR.

### F2. "< 2 s" is measured from the decision, not from launching the game

`SustainedTicksRequired` defaults to 3 at a 2 s tick, so a game is detected
about 4–6 s after it starts loading the GPU. The daemon guarantees that the
**actions** (TDP restore + FAH pause) finish inside the same tick as the
`StatePausedLoad` decision. The PENDING.md end-to-end check ("TDP 145 W and FAH
paused within 2 s") must be reworded to "within 2 s of the pause decision"
(about 6 s after launch), or `SustainedTicksRequired` must drop to 1.

**DECIDE:** reword the check (recommended; 1 tick causes false positives), or
lower the sustain window.

### F3. macOS ioreg polling conflicts with the hardware skill

`go-hardware-arch` allows `ioreg` "only during initialization or one-shot
refresh, not in a tight polling loop". A macOS daemon that detects load must
read `IOAccelerator` periodically. The same skill forbids IOKit/cgo, so `ioreg`
is the only option. Also, `FetchIOReg` has a fixed 2 s timeout and takes no
`ctx`, so two sequential calls can take 4 s, which is longer than one tick.

Proposal:
- `DarwinCollector` reads `IOAccelerator` every tick and `AppleSmartBattery`
  every 15 ticks (30 s), caching the last battery result.
- Add `FetchIORegContext(ctx, class)` and keep `FetchIOReg` as a wrapper. The
  daemon passes a per-tick deadline.
- Amend the skill rule to read: "periodic `ioreg` allowed at ≥ 2 s intervals
  through the bounded fetcher; never in a tighter loop."

**DECIDE:** approve the amendment, or choose a slower macOS tick (for example
5 s).

### F4. FAH cannot tell a user pause from a daemon pause (CH-10)

`fah.Status` only exposes `Paused` (true means paused in every group). It has no
`PausedByUser`. The rule "FAH already paused → not ours, never Resume" works in
normal operation, but it breaks after a crash. If the daemon pauses for a game,
crashes, and restarts, it sees `Paused=true`, decides the pause is not its own,
and folding stays paused forever.

Proposal: keep a **pause-ownership marker file**, written after a confirmed
`Pause` and deleted after a confirmed `Resume`. At startup, an existing marker
sets `pausedByUs=true`.
- Linux: `/run/ecofold/paused-by-daemon` (systemd `RuntimeDirectory=`; tmpfs is
  cleared on reboot, which also clears FAH's runtime pause ambiguity).
- macOS dev: `$TMPDIR/ecofold/paused-by-daemon`.
- Permissions are `0600`. The marker content is not trusted: only its presence
  matters.

Known accepted gaps (documented, not fixed):
- If the user pauses by hand while the daemon also holds a pause, the daemon
  resumes at the end of cooldown. A user resume during a game is respected (see
  §3.3).
- If `GetStatus` fails at pause time, the daemon pauses anyway (CH2-H1) and
  claims ownership, so a user pause that was hidden by the read error could be
  resumed later.

### F5. M2-BLOCK error is not a sentinel

`LinuxCollector.Collect` returns `errors.New("detector: per-process GPU
utilization not supported")`, so the daemon cannot use `errors.Is` on it.
Proposal: export `detector.ErrPerProcessUtilUnsupported` and return it. This is
a small API change in `internal/detector`, and it compiles on both platforms
because the variable lives in `detector.go`.

Note on behavior: the error appears only when graphics processes exist and all
of them lack utilization samples. On a desktop, Xorg/KWin are almost always
listed (the check runs **before** the ignore filter), so on an unsupported
driver the error is effectively permanent. Degraded is therefore **latched**
for the daemon's lifetime (see §3.4).

### F6. FAH call timeout vs the 2 s budget

`ClientV8` uses a 3 s default timeout, and `Pause`/`Resume` wait for a
confirming snapshot. The daemon must pass its own deadline:
- `GetStatus`: 500 ms
- `Pause` / `Resume`: 1500 ms (raised from 1200 ms after the BG3 run on
  CachyOS: status + pause took 1.24 s)

A timeout leaves the action **pending** and retries it on the next tick. It
never blocks the loop past the tick.

### F7. Order of actions on pause

The agreed order is "pause FAH and restore TDP in the same tick". The plan does
**TDP restore first, then FAH Pause**. NVML is local and takes milliseconds,
while FAH may be down and spend its full 1.2 s timeout. Restoring first means
the game gets 145 W even when FAH is unreachable. On resume the order stays as
agreed: lower TDP first, then `Resume`.

**DECIDE:** confirm the TDP-first order on pause.

### F8. M4 (mutex held during blocking NVML calls)

`NvidiaProvider` serializes every call behind one mutex. The daemon runs all
provider calls on **one goroutine** (the loop), so restoring at exit happens
after the loop returns and never contends with telemetry. The remaining risk is
an NVML call that never returns (a driver hang). Mitigation:
1. Shutdown waits for the loop to finish for at most `shutdownGrace` (3 s).
2. The restore runs in a goroutine bounded by a 2 s timeout. If it does not
   finish, the daemon logs and exits non-zero.
3. systemd `ExecStopPost=ecofold-daemon -restore-power-limit` runs a **fresh
   process** with its own NVML handle as the last line of defense. This is a
   one-shot NVML call, not `nvidia-smi`.

M4 stays open (a hypothesis) until it is reproduced on CachyOS. No change to
`internal/gpu` is planned.

### F9. NVML permissions: root is required (verified 2026-11-14)

NVIDIA's NVML docs say `SetPowerManagementLimit` requires "root/admin" and
returns `NVML_ERROR_NO_PERMISSION` otherwise. Third-party reports (Zeus) say the
real check is `CAP_SYS_ADMIN`. On CachyOS (RTX 5060, driver 620.70.06) a
`User=ecofold` process with `CAP_SYS_ADMIN` effective and ambient
(`CapEff=0x200000`) still got `Insufficient Permissions`; the same unit as
`User=root` works. File capabilities (`setcap`) cannot help either. The unit
therefore runs as root. `CAP_SYS_ADMIN` is close to root anyway: the value of
the systemd unit is that it drops **every other** capability
(`CapabilityBoundingSet=CAP_SYS_ADMIN`) and sandboxes the filesystem.

---

## 2. Package layout

```
internal/daemon/             platform-agnostic, fully testable on macOS
  daemon.go                  Daemon, Config, New, Run, tick, apply, shutdown
  state.go                   Mode enum + transitions
  marker.go                  pause-ownership marker (atomic write/remove)
  daemon_test.go             table + scenario tests with fakes
  fakes_test.go              fakeProvider, scriptedCollector, recordingFAH
internal/detector/
  detector.go                + ErrPerProcessUtilUnsupported, doc fixes (F1)
  linux_collector.go         return the sentinel
  darwin_collector.go        DarwinCollector (//go:build darwin), fetch seam
  darwin_ioreg.go            + FetchIORegContext (F3)
cmd/ecofold-daemon/
  main.go                    flags, slog, signal.NotifyContext, wiring
  collector_linux.go         newCollector → detector.NewLinuxCollector
  collector_darwin.go        newCollector → detector.NewDarwinCollector
deploy/systemd/
  ecofold-daemon.service     documented, unverified until CachyOS run
```

Interfaces that `internal/daemon` consumes (declared there, kept small):

```go
type Collector interface {
    Collect(ctx context.Context) (detector.SystemSnapshot, error)
}
type Evaluator interface { // *detector.Evaluator satisfies it
    Evaluate(detector.SystemSnapshot) detector.OperatingState
}
// GPU: gpu.Provider as is. FAH: fah.Controller as is.
```

`internal/daemon` imports no build-tagged code, so `go build ./...` works the
same on macOS and on Linux with or without cgo.

Flags (`cmd/ecofold-daemon`):

| Flag | Default | Notes |
| --- | --- | --- |
| `-fah-endpoint` | `fah.DefaultEndpoint` | loopback only (already validated by `NewClientV8`) |
| `-eco-watts` | `123` | clamped to the device `[Min, Max]` at startup |
| `-tick` | `2s` | bounded to `[1s, 5s]` |
| `-runtime-dir` | `/run/ecofold` on Linux, `$TMPDIR/ecofold` on macOS | marker location |
| `-restore-power-limit` | `false` | one-shot: Init → RestoreDefault → Shutdown → exit (for `ExecStopPost`) |
| `-fah-mock` | `false` | use `MockController` (macOS dev without FAH) |

Logging uses `log/slog` (text output to stderr; journald captures it). After
startup there are no `log.Fatal` or `os.Exit` calls, because they skip the
deferred restore.

---

## 3. State machine and tick

### 3.1 Daemon state

```go
type Mode string // Folding | FoldingEco | PausedLoad | PausedBattery | Degraded | Starting

type state struct {
    mode          Mode
    lastDecision  detector.OperatingState // last valid evaluator output
    pausedByUs    bool   // we own the current FAH pause (+ marker on disk)
    pendingFAH    fahOp  // none | pause | resume — retried every tick until confirmed
    capApplied    bool   // eco cap currently set by us (Linux only)
    supportsPower bool   // from GetTelemetry at startup; false on macOS/stub
    ecoWatts      int    // clamped to [MinPowerLimitW, MaxPowerLimitW]
    degraded      bool   // latched M2-BLOCK
    collectErrs   int    // consecutive Collect errors (for rate-limited logging)
}
```

### 3.2 Tick (every 2 s, single goroutine)

```
tickCtx := deadline now+1.8s
snap, err := collector.Collect(tickCtx)               // bounded ≤ 1s on Linux, ≤ 1.5s on macOS
switch {
case errors.Is(err, ErrPerProcessUtilUnsupported):   enterDegraded(); retryPendingFAH(); return
case err != nil:                                     collectErrs++; warnRateLimited(); retryPendingFAH(); return
                                                     // Evaluate is NOT called: streaks and cooldown latch are untouched (decision 5)
}
collectErrs = 0
decision := evaluator.Evaluate(snap)
if decision != lastDecision { log transition }
lastDecision = decision
apply(tickCtx, decision)
```

Note: the cooldown counts **evaluated** ticks, so error ticks lengthen the
cooldown. This is conservative and intended.

### 3.3 `apply(decision)`

```
wantPause := decision ∈ {PausedLoad, PausedBattery}

if wantPause:
    if supportsPower && capApplied:  RestoreDefaultPowerLimit(); capApplied = false   // 145 W first (F7)
    if transition into pause (prev decision was folding):
        st, err := fah.GetStatus(500ms)
        if err == nil && st.Paused:  // user already paused → not ours (CH-10)
            pendingFAH = none
        else:                        // includes idle/unknown/error (CH2-H1)
            pendingFAH = pause
else: // Folding / FoldingEco
    if supportsPower && !degraded && !capApplied:  SetPowerLimit(ecoWatts); capApplied = true   // cap first
    if pausedByUs:  pendingFAH = resume
    if pendingFAH == pause: pendingFAH = none        // decision flipped before pause landed

runPendingFAH():
    pause  → Pause(1.2s) ok  → pausedByUs = true;  writeMarker(); pendingFAH = none
    resume → Resume(1.2s) ok → pausedByUs = false; removeMarker(); pendingFAH = none
    error  → keep pendingFAH, log, retry next tick
```

Actions are **edge-triggered** on decision transitions and **retried** until
they are confirmed. The daemon does not poll FAH status every tick, so if the
user resumes by hand during a game, the daemon does not fight it: it only
pauses again on the next transition into a pause. The daemon does not notice a
manual resume, so `pausedByUs` stays true until the cooldown ends, and the
`Resume` that follows is a harmless no-op.

Power-limit errors are logged and retried on the next tick (`capApplied` stays
false). `NVML_ERROR_NO_PERMISSION` on the first `SetPowerLimit` sets
`supportsPower=false` with one clear error log, and the daemon keeps running as
Smart Pause only.

### 3.4 Degraded (M2-BLOCK)

On the first `ErrPerProcessUtilUnsupported`:
- `degraded = true` (latched) and `mode = Degraded`.
- One `slog.Warn`: driver/GPU, "per-process utilization unsupported: game
  detection disabled, power limit restored to default, folding left as is".
- If `capApplied`: `RestoreDefaultPowerLimit()`. After that, never cap again.
- FAH is not touched (no automatic pause or resume). A pending resume that the
  daemon owns is still completed, so a pause the daemon made never leaks.

### 3.5 Startup

1. `provider.Init()`. On error on Linux, exit non-zero: without NVML there is
   no game detection, and systemd `Restart=on-failure` retries with a backoff.
   On macOS the mock always succeeds.
2. `GetTelemetry()` → `supportsPower`, clamp `ecoWatts` to `[Min, Max]` and log
   the effective value.
3. Read the marker → `pausedByUs`.
4. Open the FAH controller. A dial failure is **not** fatal: actions stay
   pending and retry.
5. Register the deferred fail-safe (§3.6), then start the ticker. No power or
   FAH action happens before the first successful `Collect` + `Evaluate`.

### 3.6 Shutdown and fail-safe

```
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
// SIGINT == os.Interrupt on Unix; both are covered
defer failSafe()  // runs on return and after recover() in the loop goroutine
```

- The loop goroutine has `defer func(){ if r := recover(); r != nil { log; errCh <- ... } }()`.
- `failSafe()`: if `supportsPower`, call `RestoreDefaultPowerLimit()` in a goroutine with a 2 s
  bound (F8). Then `fah.Close()` and `provider.Shutdown()`.
- FAH on exit: **do not resume**. If a game is running, resuming would take the
  GPU from it. The marker stays, so the next start owns the pause and resumes
  it after cooldown. **DECIDE:** confirm (recommended), or resume on clean exit
  when the last decision was not a pause.
- Panics in other goroutines, `SIGKILL`, OOM and fatal runtime errors cannot
  run Go code. For those, `ExecStopPost` is the only protection.

---

## 4. Test plan (macOS, `go test -race ./...`)

Fakes, all in `internal/daemon` test files:
- `fakeProvider`: `supports` flag, Min/Max/Default, records every call in an
  ordered log shared with the FAH fake, injects errors, and has an optional
  blocking `Restore` for M4.
- `scriptedCollector`: returns a scripted sequence of `(snapshot, error)`.
- `recordingFAH`: wraps `fah.MockController` and adds call recording,
  per-method error injection and a configurable initial `Paused`.
- The tests drive `d.tick(ctx)` directly, without real time. One test runs
  `Run` with a 10 ms tick to cover signal and `ctx` cancellation.

| # | Scenario | Asserts |
| --- | --- | --- |
| T1 | macOS mode (`supports=false`): folding → load → cooldown → folding | zero `SetPowerLimit`/`Restore` calls; one Pause, one Resume |
| T2 | Linux mode: first Folding tick | `SetPowerLimit(123)` once, not repeated on later ticks |
| T3 | Linux: load decision | same tick: `Restore` **before** `Pause` (ordered log) |
| T4 | Linux: cooldown ends | `SetPowerLimit` **before** `Resume` |
| T5 | Battery low (darwin snapshot) → `PausedBattery` → AC back | Pause then Resume, no cooldown wait |
| T6 | CH-10: FAH already paused when load starts | no Pause sent; no Resume after cooldown; no marker written |
| T7 | CH2-H1: status `idle`, `unknown`, or `GetStatus` error | Pause is still sent |
| T8 | Transient `Collect` error mid-cooldown | Evaluate not called; latch kept; no FAH/power calls; cooldown completes N valid ticks later |
| T9 | Transient error during sustain window | high-load streak is not reset |
| T10 | M2-BLOCK sentinel | Degraded latched; `Restore` called once; never `SetPowerLimit` again; one warning; owned resume still completes |
| T11 | Pause fails (FAH down) then recovers | retried every tick; `pausedByUs` only after success; marker only after success |
| T12 | Decision flips back before pending pause lands | pending pause dropped, no Resume |
| T13 | Marker present at startup | `pausedByUs=true`; Resume after first folding decision; marker removed |
| T14 | ctx cancel (SIGTERM path) | `Restore` called once; FAH closed; provider shut down; no Resume |
| T15 | Panic inside collector | recovered; `Restore` called; `Run` returns error |
| T16 | Hung `Restore` (M4) | shutdown returns within grace + 2 s with error |
| T17 | `ecoWatts` outside `[Min,Max]` / `SetPowerLimit` NO_PERMISSION | clamped value used / `supports=false` after one error |
| T18 | `DarwinCollector` with `testdata/m4_air_*.plist` via fetch seam | snapshot fields; battery cached for 15 ticks; fetch error → error (no empty snapshot) |
| T19 | Real `MockProvider` + `MockController` end-to-end on darwin | daemon runs N ticks with no power calls and no error |

Also: `go vet ./...` and `CGO_ENABLED=0 GOOS=linux go build ./...` (stub
compile check). Baseline on 2026-11-13: all three pass on `develop`.

---

## 5. Failure matrix (Linux, verify on CachyOS)

| Failure | Daemon behavior | Last line of defense |
| --- | --- | --- |
| NVML `Init` fails | exit ≠ 0, no actions | systemd `Restart=on-failure` |
| `SetPowerLimit` → `NO_PERMISSION` | `supports=false`, Smart Pause only, one error log | — |
| `GetTelemetry`/`GetGraphicsProcesses` error | keep last decision, retry | — |
| Per-process util unsupported | Degraded (latched), 145 W, FAH untouched | — |
| GPU lost / driver reload | Collect errors → hold; restore attempts logged | NVML limits reset on driver reload (verify) |
| NVML call hangs (M4) | shutdown grace 3 s + restore bound 2 s → exit ≠ 0 | `ExecStopPost -restore-power-limit` |
| Panic in loop | recover → restore → exit ≠ 0 | `ExecStopPost` + restart |
| SIGKILL / OOM / fatal error | nothing runs | `ExecStopPost` |
| FAH down / restarting | power logic unaffected; FAH action pending + retried | — |
| Daemon crash while it holds the pause | marker in `/run/ecofold` | next start resumes after cooldown |
| Reboot | `/run` cleared, NVML limit back to default | — |

---

## 6. systemd unit (verified on CachyOS 2026-11-14)

```ini
[Unit]
Description=EcoFold GPU power daemon
After=nvidia-persistenced.service docker.service
Wants=nvidia-persistenced.service

[Service]
Type=exec
ExecStart=/usr/bin/ecofold-daemon
ExecStopPost=/usr/bin/ecofold-daemon -restore-power-limit
Restart=on-failure
RestartSec=5
TimeoutStopSec=10

User=root
Group=root
CapabilityBoundingSet=CAP_SYS_ADMIN
NoNewPrivileges=yes

RuntimeDirectory=ecofold
RuntimeDirectoryMode=0750
RuntimeDirectoryPreserve=restart
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
DeviceAllow=/dev/nvidiactl rw
DeviceAllow=/dev/nvidia0 rw
DeviceAllow=/dev/nvidia-uvm rw
DevicePolicy=closed

[Install]
WantedBy=multi-user.target
```

Results on CachyOS (RTX 5060, driver 620.70.06, 2026-11-14). Each item below
was checked by a script that installed the unit and drove it with `systemctl`:

| # | Check | Result |
| --- | --- | --- |
| 1 | `SetPowerManagementLimit` as `User=ecofold` + `CAP_SYS_ADMIN` | FAIL (`Insufficient Permissions`); PASS as `User=root` (F9) |
| 2 | `/proc/<pid>/comm` and `cmdline` of user processes under `ProtectSystem=strict` | PASS (read from the service's mount namespace) |
| 3 | `ExecStopPost` after `systemctl kill -s KILL` | PASS: 145 W within 1 s |
| 4 | `RuntimeDirectoryPreserve=restart` across `Restart=` | PASS: `/run/ecofold` survived |
| 5 | `DeviceAllow` (`nvidiactl`, `nvidia0`, `nvidia-uvm`) | PASS: no device errors |
| 6 | End to end with Baldur's Gate 3 | PASS: `paused_load` + 145 W + FAH paused through the API; 2 min after closing the game, 123 W and FAH folding |

Found during check 6: the legacy `autofold.service` runs as a **user** unit
(`~/.config/systemd/user/`), so `systemctl is-active autofold` (system scope)
misses it. With both running, autofold froze the FAH container with
`docker pause` and EcoFold's API calls timed out. Packaging must disable it
(`systemctl --user disable --now autofold`).

Original checklist, in order:
1. `SetPowerManagementLimit` works as `User=ecofold` with only
   `CAP_SYS_ADMIN`. If it returns `NO_PERMISSION`, fall back to `User=root`
   and keep the `CapabilityBoundingSet` and sandbox lines.
2. `/proc/<pid>/comm` and `cmdline` of the user's game are readable under
   `ProtectSystem=strict` (no `ProtectProc=invisible`, no `hidepid`).
3. `ExecStopPost` runs after `systemctl kill -s KILL ecofold-daemon` and the
   limit returns to 145 W.
4. `RuntimeDirectoryPreserve=restart` keeps the marker across `Restart=`.
5. `DeviceAllow` paths (`ls /dev/nvidia*`) for the RTX 5060.
6. End to end with *Deadlock*, per F2.

---

## 7. Implementation order (after approval)

1. `detector`: sentinel + doc fixes (F1, F5) + `FetchIORegContext` +
   `DarwinCollector` with T18.
2. `internal/daemon`: state, tick, apply, marker, with T1–T17 written first.
3. `cmd/ecofold-daemon`: flags, wiring, `-restore-power-limit`, T19 smoke.
4. `deploy/systemd/ecofold-daemon.service`, docs updates (`PENDING.md`
   checkboxes, `ARCHITECTURE.md`, skill amendment if F3 is approved).
5. Commits per `git-workflow-standards` (`feat(detector)`, `feat(daemon)`,
   `docs(daemon)`); PR to `develop` linked to issue #3.

Not in this branch: the Go 1.27.4 upgrade (needed before the IPC socket, step
3), the Proton/Wine watcher (M2 layer 2), and R3/R4.

## Sources

- NVML Device Commands (root/admin requirement, `NVML_ERROR_NO_PERMISSION`):
  https://docs.nvidia.com/deploy/nvml-api/group__nvmlDeviceCommands.html
- Zeus issue on `SYS_ADMIN` for NVML power limits:
  https://github.com/ml-energy/zeus/issues/29
- NVIDIA forum, non-root power limit:
  https://forums.developer.nvidia.com/t/giving-a-non-root-user-permission-to-set-powerlimit/69373
