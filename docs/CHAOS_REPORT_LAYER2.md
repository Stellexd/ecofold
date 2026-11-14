# Chaos Report: Detection Layer 2 and Startup Hold

Date: 2026-11-14

## 1. Scope

| Item | Value |
| --- | --- |
| Commit tested | `52f5d4d` (`develop`, "Merge pull request #19 from Stellexd/feat/m2-proton-watcher") |
| Branch | `test/m2-layer2-chaos` |
| Platforms | macOS Apple Silicon (`darwin/arm64`, Go 1.27.3); CachyOS (`linux/amd64`, Go 1.27.4, `CGO_ENABLED=1`, RTX 5060) |
| Code under test | `internal/detector/linux_sessions.go` (game sessions), the session hold in `Evaluator`, and the inherited-pause startup hold in `internal/daemon` |
| Rules | Unprivileged only; no hardware state was changed. |

## 2. Techniques and results

| Technique | What it checks | Result |
| --- | --- | --- |
| Session property model (`TestChaosSessionModel`, 200 seeds × 2000 ticks) | Random load, GPU-process session tags and live sets against S1–S4 below | PASS |
| Native fuzzing (`FuzzEvaluatorSessions`) | Same properties, coverage-guided | 1 min + 2 min on macOS, about 15 M execs, no violation |
| Hostile `/proc` trees (`TestChaosCollectorProcTree`, 300 seeds; `FuzzCollectorProcTree`) | Up to 40 processes with random parents (cycles, self-parents), zombies, malformed or missing `stat`, missing or empty `cmdline`, 300-byte argv, `SteamLaunch` after `--`, and reads that start failing with ENOENT mid-walk | PASS; 3 min on CachyOS, 12.7 M execs, no panic or violation |
| Read budget | `/proc` reads per tick stay under `procs × (3 + 2 × 33) + 32 + 1` | PASS |
| Tracked-session bound (`TestChaosTrackedSessionsBounded`) | 96 concurrent `.exe` games | Tracked set stays at 32 |
| Spoofed reaper (`TestSpoofedReaperOnlyHoldsItsOwnLoad`) | A user process named `reaper SteamLaunch` | Becomes a session but never pauses on its own (accepted, see L2-1) |
| Daemon model (`TestChaosModelRandomSequences`, `FuzzDaemonInvariants`) with new invariants I7–I8 and game-session events (load in a session, idle in its menu) | Restarts and crashes during a game session | PASS; 100 seeds, fuzz 3 min (about 500 k execs) |
| Mutation checks | The models must catch real regressions | Holding on *any* live session fails S2 (tick 1578, seed 0); reverting the startup-hold power fix fails I7 |
| Benchmark (`BenchmarkCollectSessions`, CachyOS) | One tick with 10 GPU processes, each 8 levels below a reaper | 74 µs/op with the in-memory fake (1155 allocs); real `/proc` reads add a few µs each, well under the 2 s tick |
| Full suite | `go vet`, `gofmt`, `go test -race ./...` on macOS and CachyOS | PASS |

Properties of the session model:

| ID | Property |
| --- | --- |
| S1 | A pause starts only after `SustainedTicksRequired` consecutive load ticks; sessions never start one. |
| S2 | After `CooldownTicksRequired` consecutive ticks with no load and no live session that caused load, the evaluator is not paused. |
| S3 | The held set never exceeds `maxHeldSessions`. |
| S4 | With no sessions in the input, the result equals a session-free evaluator. |

New daemon invariants (added to I1–I6 from `CHAOS_REPORT_M3.md`):

| ID | Invariant |
| --- | --- |
| I7 | While an inherited pause is in its startup hold, the daemon never calls `SetPowerLimit` (a game may still be running). |
| I8 | An inherited pause is never resumed before the startup hold ends. |

## 3. Findings

No defects found. One accepted risk:

### L2-1 (Low, accepted): any user process can pose as a Steam session

A process named `reaper` with `SteamLaunch` in its argv becomes a session for
its descendants. It cannot start a pause, and it only holds a pause caused by
its own GPU load, for as long as it runs. The same user can get that by
pausing FAH by hand, so it does not cross a privilege boundary.
