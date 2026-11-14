# Pending Work

Open work only, ordered by priority. Tick each box in the PR that completes
it, then move it to `docs/archive/PENDING_HISTORY.md` when the section is
done. Finished work and its context (M1 review, chaos passes, daemon loop):
`docs/archive/PENDING_HISTORY.md` and the reports in `docs/`.

Last updated: 2026-11-14 (step 1 verified on CachyOS: the unit runs as root;
results in `DAEMON_PLAN.md` section 6).

## Road to v0.1.0

Each step is its own branch and PR to `develop`.

### 1. Verify the systemd unit on CachyOS (needs root)

Unit: `deploy/systemd/ecofold-daemon.service`. Verified 2026-11-14; details in
`DAEMON_PLAN.md` section 6.

- [x] `SetPowerManagementLimit` as `User=ecofold` with only `CAP_SYS_ADMIN`:
  NVML returns `Insufficient Permissions`. The unit now runs as `User=root`
  and keeps `CapabilityBoundingSet=CAP_SYS_ADMIN` and the sandbox.
- [x] `ExecStopPost` restores 145 W after `systemctl kill -s KILL`.
- [x] `/proc/<pid>/comm` and `cmdline` of user processes are readable under
  `ProtectSystem=strict`; `DeviceAllow` paths match `ls /dev/nvidia*`.
- [x] `RuntimeDirectoryPreserve=restart` keeps `/run/ecofold` across
  `Restart=`, but not across `stop` + `start`: a reinstall during a game left
  FAH paused for good. Now `RuntimeDirectoryPreserve=yes`, verified on
  CachyOS with a stop/start during Deadlock. The restarted daemon also keeps
  the default limit and waits a full cooldown before resuming an inherited
  pause; re-check on hardware with the next binary.
- [x] Closing Baldur's Gate 3 ends the 2 min cooldown with 123 W and FAH
  folding.

### 2. Close M2 (issue #2): Proton/Wine watcher and app calibration

Branch `feat/m2-proton-watcher`.

- [x] Detection layer 2 (`internal/detector/linux_sessions.go`): GPU
  processes are tied to a game session (ancestor `reaper SteamLaunch`, or a
  Wine `.exe` outside Steam) through `/proc/<pid>/stat` and `cmdline`, no shell
  commands. Verified on CachyOS with Deadlock: `deadlock.exe` and Wine's
  `explorer.exe` map to `steam:1422450`. `wineserver` is not used: it is not
  an ancestor of the game and outlives it.
- [x] Share the existing cooldown latch with layer 1: a session holds the load
  pause it caused while it is live, and never starts one.
- [x] Filter Electron helpers: `electron`, `discord`, `slack`, `code` and
  `spotify` added to `browserBinaries` in `linux_collector.go`, so
  `isBrowserHelper` drops their `--type=renderer` and `--type=gpu-process`
  processes (`fix/m2-detector-bounds`). Found on CachyOS: Discord's renderer
  is listed by NVML as a graphics process. Verify on hardware during the
  matrix run.
- [x] Enumerate compute processes too (`GetComputeRunningProcesses`): CUDA
  work (Blender Cycles/OptiX, DaVinci Resolve, local AI) never appears as a
  graphics process, so it could not pause folding under the creative-app
  policy. Found during calibration; the FAH core now shows up (89-94 % SM)
  and stays ignored.
- [ ] Run the app compatibility matrix below and calibrate the thresholds
  (45 % / 25 % / 1500 MiB); update `internal/detector/README.md`.
- [x] **R3** and **R4** fixed in `fix/m2-detector-bounds` (see "Open risks").

#### App compatibility matrix

Goal: every app falls into the right class, with measured numbers behind the
ignore list and the thresholds.

| Class | Expected daemon behavior |
| --- | --- |
| **Ignore**: launchers, chat, browsers, Electron/CEF helpers | Never pause, even with VRAM held |
| **Load**: games | Pause after the 3-tick sustain, resume after cooldown |
| **Load**: creative/GPU-compute apps under sustained load | Same as games; folding continues while they sit idle below the thresholds |

| App | Class | Where to test | Notes |
| --- | --- | --- | --- |
| Discord (Electron) | Ignore | CachyOS | Renderer seen in NVML; filter pending |
| Steam client / steamwebhelper | Ignore | CachyOS | Already ignored |
| Riot Client / League of Legends launcher | Ignore (launcher) / Load (game) | Windows only | Riot Vanguard blocks Linux, so EcoFold never sees LoL on Linux; measure only |
| Epic Games Launcher, Battle.net, EA app (via Proton/Lutris/Heroic) | Ignore | CachyOS | CEF launchers; check process names under Wine |
| Adobe Photoshop | Load | macOS (native, `DarwinCollector`); Windows to measure | No native Linux build |
| Adobe Premiere Pro (export, playback) | Load | macOS (native); Windows to measure | No native Linux build |
| DaVinci Resolve, Blender (render) | Load | CachyOS (native) | Native Linux NVIDIA apps: the best real test for the creative "Load" class |
| OBS (NVENC recording) | Load | CachyOS | Uses the encoder more than SM; check `enc` vs `sm` |
| Baldur's Gate 3 | Load | CachyOS | Verified 2026-11-13: 55-59 % SM, paused |
| Crimson Desert, Deadlock | Load | CachyOS | Pending |
| Video playback in a browser | Ignore | CachyOS, macOS | Hardware decode must not pause |

How to measure:

- **CachyOS and macOS (EcoFold runs):** run the daemon with `-debug` and,
  on Linux, `nvidia-smi pmon -s um -c 30` next to it. Record process name, SM
  %, VRAM and the daemon's decision.
- **Windows dual boot (EcoFold does not run on Windows):** only collect data
  with `nvidia-smi pmon -s um -c 60` (idle, menu, active use) for apps that
  have no Linux version. Use it to set thresholds and process names; it does
  not test the daemon. A Windows port is out of scope for `v0.1.0`.

Decision (2026-11-14): heavy creative and productivity apps (Blender,
Premiere, Resolve, Photoshop) **pause folding** when they put sustained load
on the GPU (rendering, exporting, 3D/compute acceleration) above the
evaluator thresholds, exactly like a game. While they are open but idle below
the utilization threshold, folding continues. No games-only list: the
evaluator stays app-agnostic and the user's foreground work wins. On Linux,
Chromium/Electron/CEF helpers that are not real load (Discord, Slack, VS Code,
Spotify) are filtered through `browserBinaries` / `DefaultIgnoredProcesses`;
calibrate the thresholds with this policy in mind.

### 3. M3 (issue #3): IPC and GUI

- [ ] Upgrade both machines and `go.mod` to Go 1.27.4 (GO-2026-6859) before any
  socket exists.
- [ ] Unix domain socket with restrictive permissions (owner/group only) and a
  minimal protocol: status, force pause/resume, read/update config.
- [ ] Security review of the socket before merging (new attack surface). The
  daemon runs as root, so a socket bug is a root bug: socket `0660` with an
  `ecofold` group, check `SO_PEERCRED`, cap message size, validate watts
  against the NVML constraints.
- [ ] Harden the unit further (`SystemCallFilter=@system-service`,
  `ProtectKernelModules`, `RestrictNamespaces`, `MemoryDenyWriteExecute`) and
  re-check that NVML and cgo still work on CachyOS.
- [ ] Add Wails to `go.mod`; system tray with state (Folding / Paused by game /
  Battery) and current TDP. The GUI stays unprivileged.
- [ ] GUI handles the daemon being down (shows "daemon not running", no crash).

### 4. M4 (issue #4): CI, packaging and release

- [ ] CI in `.github/workflows`: build, `go test -race`, `go vet` and `gofmt`
  on Linux (CGO) and macOS.
- [ ] `govulncheck` in CI.
- [ ] Renovate targeting `develop` (`baseBranches`), with `config:recommended`
  and `gomodTidy`; `renovate.json` lives on `main`.
- [ ] AUR `PKGBUILD`: daemon, systemd unit and GUI. Declare the legacy
  autofold setup in `conflicts` and document in the README that
  `autofold.service` must be disabled. It is a **user** unit:
  `systemctl --user disable --now autofold`. With both running, autofold
  freezes the FAH container (`docker pause`) and EcoFold cannot reach the
  API.
- [ ] macOS `.dmg` (telemetry + FAH control only; no TDP on Apple Silicon).
- [ ] README with install, permissions and known limitations.
- [ ] Tag `v0.1.0` on `main` once every box above is ticked.

## Open risks

- [x] **R3 `GPUUtilThresholdPct = 100` plus a huge `GPUMemThresholdMiB`.**
  Fixed: `NewEvaluator` resets `GPUMemThresholdMiB > 65536` (64 GiB) to 1500.
  Pinned by `TestMaxUtilThresholdPlusHugeMemStillPauses` and
  `TestMemThresholdUpperBound`.
- [x] **R4 Transient empty `AppleSmartBattery` output folds at full power.**
  Fixed: once a read reports `HasBattery=true`, `DarwinCollector` returns
  `ErrBatteryVanished` for a later empty array, so the daemon keeps its last
  decision and retries. Pinned by
  `TestDarwinCollectorBatteryVanishedIsTransient`.
- [ ] **M2-BLOCK fallback.** Drivers without per-process utilization now
  latch the daemon into `Degraded` (no game detection, default limit). A
  fallback detector would restore game detection on those systems. Layer 2
  can help (a session plus `GPUMemThresholdMiB` of VRAM), but it needs the
  daemon to keep collecting in `Degraded` mode instead of latching.
- [ ] **M4 Mutex held during blocking NVML calls.** Still a hypothesis. The
  daemon bounds the exit restore and `ExecStopPost` is the backstop; reproduce
  on hardware before changing `internal/gpu`.
- [ ] **R3-1, R3-2** (accepted, see `CHAOS_REPORT_M3.md`): an unwritable
  runtime dir loses pause ownership across restarts; any entry at the marker
  path counts as ownership (root-only).

## After v0.1.0

- [ ] Evaluate a minimal privileged helper (`ecofold-powerctl`, started
  through polkit/`pkexec`) so the daemon itself can run unprivileged. Today
  NVML requires uid 0 for power limits.

## Housekeeping

- [ ] Publish the FAH container on `127.0.0.1:7396:7396` instead of
  `0.0.0.0:7396`: today any host on the LAN can pause or reconfigure FAH.
- [x] Delete merged branches (`test/m1-chaos*`, `test/m1-nvml-integration`,
  `revert-8-test/m1-nvml-integration`, `fix/m1-ch11-nvml-lifecycle`) and the
  agent worktrees in `.claude/worktrees/` (done 2026-11-14 on macOS). The
  CachyOS checkout `~/Documents/GitHub/ecofoldGPU` still sits on
  `fix/m1-ch11-nvml-lifecycle`: switch it to `develop` and delete the branch
  there.
