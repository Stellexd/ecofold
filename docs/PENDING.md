# Pending Work

Open work only, ordered by priority. Tick each box in the PR that completes
it, then move it to `docs/archive/PENDING_HISTORY.md` when the section is
done. Finished work and its context (M1 review, chaos passes, daemon loop):
`docs/archive/PENDING_HISTORY.md` and the reports in `docs/`.

Last updated: 2026-11-13 (after the daemon chaos pass, `CHAOS_REPORT_M3.md`).

## Road to v0.1.0

Each step is its own branch and PR to `develop`.

### 1. Verify the systemd unit on CachyOS (needs root)

Unit draft: `deploy/systemd/ecofold-daemon.service`.

- [ ] `SetPowerManagementLimit` works as `User=ecofold` with only
  `CAP_SYS_ADMIN`; otherwise fall back to `User=root` and keep the bounding
  set and sandbox.
- [ ] `ExecStopPost` restores 145 W after `systemctl kill -s KILL`.
- [ ] `/proc/<pid>/comm` and `cmdline` of user games are readable under
  `ProtectSystem=strict`; `DeviceAllow` paths match `ls /dev/nvidia*`.
- [ ] `RuntimeDirectoryPreserve=restart` keeps the pause marker across
  `Restart=`.
- [ ] The in-process cooldown after closing a real game (2 min) ends with
  123 W and FAH folding (only the marker path was verified on hardware).

### 2. Close M2 (issue #2): Proton/Wine watcher and app calibration

Branch `feat/m2-proton-watcher`.

- [ ] Detection layer 2: `wineserver`, `reaper` and `.exe` payloads as a game
  signal, reading `/proc` once per tick (no shell commands).
- [ ] Share the existing cooldown latch with layer 1.
- [ ] Filter Electron helpers: add `discord` and other Electron binaries to
  `browserBinaries` in `linux_collector.go`, so `isBrowserHelper` drops their
  `--type=renderer` and `--type=gpu-process` processes. Found on CachyOS:
  Discord's renderer is listed by NVML as a graphics process.
- [ ] Run the app compatibility matrix below and calibrate the thresholds
  (45 % / 25 % / 1500 MiB); update `internal/detector/README.md`.
- [ ] **R3** and **R4** decided or fixed (see "Open risks").

#### App compatibility matrix

Goal: every app falls into the right class, with measured numbers behind the
ignore list and the thresholds.

| Class | Expected daemon behavior |
| --- | --- |
| **Ignore**: launchers, chat, browsers, Electron/CEF helpers | Never pause, even with VRAM held |
| **Load**: games | Pause after the 3-tick sustain, resume after cooldown |
| **Load?**: creative/GPU-compute apps | Decision pending (see below) |

| App | Class | Where to test | Notes |
| --- | --- | --- | --- |
| Discord (Electron) | Ignore | CachyOS | Renderer seen in NVML; filter pending |
| Steam client / steamwebhelper | Ignore | CachyOS | Already ignored |
| Riot Client / League of Legends launcher | Ignore (launcher) / Load (game) | Windows only | Riot Vanguard blocks Linux, so EcoFold never sees LoL on Linux; measure only |
| Epic Games Launcher, Battle.net, EA app (via Proton/Lutris/Heroic) | Ignore | CachyOS | CEF launchers; check process names under Wine |
| Adobe Photoshop | Load? | macOS (native, `DarwinCollector`); Windows to measure | No native Linux build |
| Adobe Premiere Pro (export, playback) | Load? | macOS (native); Windows to measure | No native Linux build |
| DaVinci Resolve, Blender (render) | Load? | CachyOS (native) | Native Linux NVIDIA apps: the best real test for the "Load?" class |
| OBS (NVENC recording) | Load? | CachyOS | Uses the encoder more than SM; check `enc` vs `sm` |
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

Open decision: should heavy creative/compute apps (Premiere export,
Photoshop GPU filters, Blender renders) pause folding? Today any non-ignored
process above the thresholds pauses it, which favors the user's foreground
work. The alternative is a games-only policy with an opt-in list. Decide
before calibrating.

### 3. M3 (issue #3): IPC and GUI

- [ ] Upgrade both machines and `go.mod` to Go 1.27.4 (GO-2026-6859) before any
  socket exists.
- [ ] Unix domain socket with restrictive permissions (owner/group only) and a
  minimal protocol: status, force pause/resume, read/update config.
- [ ] Security review of the socket before merging (new attack surface).
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
  `autofold.service` must be disabled.
- [ ] macOS `.dmg` (telemetry + FAH control only; no TDP on Apple Silicon).
- [ ] README with install, permissions and known limitations.
- [ ] Tag `v0.1.0` on `main` once every box above is ticked.

## Open risks

- [ ] **R3 `GPUUtilThresholdPct = 100` plus a huge `GPUMemThresholdMiB`.**
  Both values are in range, but together 99 % utilization never pauses.
  Pinned by `TestMaxUtilThresholdPlusHugeMemDisablesPause`. Decide an upper
  bound for `GPUMemThresholdMiB` or the utilization threshold.
- [ ] **R4 Transient empty `AppleSmartBattery` output folds at full power.**
  An empty array is treated as a desktop Mac. Cache `HasBattery` from the
  first successful read and treat a later empty result as an error.
- [ ] **M2-BLOCK fallback.** Drivers without per-process utilization now
  latch the daemon into `Degraded` (no game detection, default limit). A
  fallback detector (process-name heuristic from layer 2) would restore game
  detection on those systems.
- [ ] **M4 Mutex held during blocking NVML calls.** Still a hypothesis. The
  daemon bounds the exit restore and `ExecStopPost` is the backstop; reproduce
  on hardware before changing `internal/gpu`.
- [ ] **R3-1, R3-2** (accepted, see `CHAOS_REPORT_M3.md`): an unwritable
  runtime dir loses pause ownership across restarts; any entry at the marker
  path counts as ownership (root-only).

## Housekeeping

- [ ] Publish the FAH container on `127.0.0.1:7396:7396` instead of
  `0.0.0.0:7396`: today any host on the LAN can pause or reconfigure FAH.
- [ ] Delete merged branches (`test/m1-chaos*`, `test/m1-nvml-integration`,
  `revert-8-test/m1-nvml-integration`, `fix/m1-ch11-nvml-lifecycle`) and the
  agent worktrees in `.claude/worktrees/`.
