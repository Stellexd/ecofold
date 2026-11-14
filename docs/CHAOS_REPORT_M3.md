# Chaos Report: Daemon Control Loop (`internal/daemon`)

Date: 2026-11-13

## 1. Scope

| Item | Value |
| --- | --- |
| Commit tested | `44a3c51` (`develop`, "Merge pull request #14 from Stellexd/feat/m3-daemon-loop") |
| Branch | `test/m3-daemon-chaos` |
| Platforms | macOS Apple Silicon (`darwin/arm64`, Go 1.27.3); CachyOS (`linux/amd64`, Go 1.27.3, `CGO_ENABLED=1`, RTX 5060, FAH v8.5.6 in Docker) |
| Packages | `internal/daemon` and `cmd/ecofold-daemon`. M1 packages were not re-tested (two chaos passes already covered them). |
| Rules | Unprivileged only: the real GPU power limit was never changed. The live FAH test paused, froze and restarted the real container and left it folding. |

## 2. Techniques and results

| Technique | What it checks | Result |
| --- | --- | --- |
| Property model (`TestChaosModelRandomSequences`, 300 seeds × 400 events during the pass, 100 seeds in the normal suite) | Random sequences of load, battery, `Collect` errors, `ErrPerProcessUtilUnsupported`, FAH errors, GPU errors (including NO_PERMISSION), manual user pause/resume, clean restarts and crashes, against invariants I1–I6 below | **Found CH3-1**; clean after the fix |
| Native fuzzing (`FuzzDaemonInvariants`, same invariants) | Coverage-guided event sequences | 3 min + 1 min, about 230 k execs, no violation |
| Random cancellation (`TestChaosRunCancelAtRandomPoints`, 200 iterations) | `Run` canceled at random points with 0–3 ms GPU and FAH latency: returns promptly, restores once, `Shutdown` is the last call, no FAH call after `Close` | PASS |
| `go test -race -count=200 -run 'TestRun\|TestChaosRun'` | Flakes and races in the shutdown paths | PASS (108 s) |
| Marker attacks (`TestChaosMarker*`) | Symlink at the marker path, marker path is a directory, runtime dir is a file, read-only runtime dir | PASS; two residual risks (R3-1, R3-2) |
| Live FAH (`TestLiveFAHFrozenAndRestarted`, `-tags fahlive`, CachyOS) | Container frozen with `docker pause` under load, then thawed; `docker restart` in the middle of the cooldown | PASS (see 2.1) |
| `golangci-lint` with `gosec,errcheck,staticcheck,gocritic` | `internal/daemon`, `cmd/ecofold-daemon` | 7 issues, all in test files (weak RNG, test file permissions, test `ReadFile`): false positives. Production code clean. |
| Full suite | `go test -race ./...` on macOS and CachyOS (CGO), `go vet` (also `-tags fahlive` for Linux), `gofmt` | PASS |

Invariants checked after every step of the model:

| ID | Invariant |
| --- | --- |
| I1 | A pause the user made is never resumed (CH-10). The documented gaps in `DAEMON_PLAN.md` F4 are modeled explicitly and excluded. |
| I2 | `Resume` runs only with the eco cap in place, unless power control is off (unsupported, NO_PERMISSION or Degraded). |
| I3 | The daemon never loses track of a cap it applied (`limit == eco ⇒ capApplied`). |
| I4 | While the decision is a pause, the GPU is at the default limit unless the last restore failed. |
| I5 | Ownership in memory matches the marker on disk. |
| I6 | After a clean exit, the GPU is at the default limit. |

### 2.1 Live FAH run on CachyOS

| Step | Observation |
| --- | --- |
| FAH frozen (`docker pause`) + load | The loop kept ticking (7 ticks in 8 s, each bounded by the 0.5 s status + 1.5 s action timeouts). `GetStatus` timed out and the daemon paused anyway (CH2-H1). One rate-limited warning. No ownership taken without confirmation. |
| `docker unpause` | Pending pause confirmed 0.8 s later. |
| Idle, then `docker restart` mid-cooldown | Cooldown completed (60 ticks at 250 ms) and folding resumed 4.8 s after the restart. FAH left folding. |

## 3. Findings

### CH3-1 (Medium, fixed): unknown limit after a failed startup restore is treated as an applied cap

- **Where:** `internal/daemon/daemon.go`, `start` and `applyCap`.
- **Repro (minimal sequence found by the model):** load → pause; `RestoreDefaultPowerLimit` starts failing; clean restart (startup restore fails, so `capApplied=true` "to be safe"); idle.
- **Effect:** `applyCap` saw `capApplied=true` and skipped `SetPowerLimit`. FAH resumed and kept folding at whatever limit the GPU had (145 W in the model) until the next pause. This breaks F7 (resume only after the cap is applied) and the whole point of EcoFold. It is not a safety issue: the GPU gets more power, not less.
- **Fix:** a separate `limitUnknown` flag. A failed startup restore still forces a restore on the first pause (`capApplied=true`), but `applyCap` now sets the eco cap when the limit is unknown. Pinned by `TestStartupRestoreFailureStillCapsWhileFolding`.

### R3-1 (Low, residual): unwritable runtime dir loses pause ownership across restarts

If the marker cannot be written (read-only or full runtime dir), the daemon
keeps ownership in memory and logs `could not write the pause marker`. A
restart then forgets the pause, and FAH stays paused until the user resumes
it. In production the dir is `/run/ecofold` (tmpfs, created by systemd), so
this needs a broken host. Accepted; the warning is the signal.

### R3-2 (Low, residual): any entry at the marker path counts as ownership

`present` uses `Lstat`, so a directory or symlink at
`/run/ecofold/paused-by-daemon` makes the daemon claim the current FAH pause
and resume it after cooldown. Writing there requires root (the dir is
`0750 root`), and `set`/`clear` never follow a symlink (verified). Accepted.

### Observations (no change)

- With FAH frozen, each tick takes about 2 s (status + action timeouts), so
  load decisions slow down to one per 2 s. The power limit is still restored
  before any FAH call, so the game is not affected.
- The cooldown counts evaluated ticks; error ticks lengthen it (by design,
  see `DAEMON_PLAN.md` §3.2).

## 4. Not covered

- The systemd unit (`CAP_SYS_ADMIN`, `ExecStopPost` after SIGKILL): needs root
  on CachyOS; it is step 1 of "Road to v0.1.0" in `PENDING.md`.
- A real hung NVML call (M4): still not reproducible without a faulty driver.
  The bounded restore is covered by `TestRunBoundsHungRestore` with a fake.
- Oscillation around the load thresholds is covered by the evaluator's own
  tests (`detector_cooldown_test.go`); the model only checks the daemon's
  invariants on top of it.

## 5. How to rerun

```
go test -race ./internal/daemon/                                  # model (100 seeds), cancellation, marker
go test -run '^$' -fuzz FuzzDaemonInvariants -fuzztime 3m ./internal/daemon/
go test -race -count=200 -short -run 'TestRun|TestChaosRun' ./internal/daemon/
go test -tags fahlive -run TestLiveFAH -v ./internal/daemon/      # CachyOS, real FAH container
```
