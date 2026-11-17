# Pending Work

Open work only, ordered by priority. Tick each box in the PR that completes
it, then move it to `docs/archive/PENDING_HISTORY.md` when the section is
done. Finished work and its context (M1 review, chaos passes, daemon loop,
systemd verification, layer 2, compute contention):
`docs/archive/PENDING_HISTORY.md` and the reports in `docs/`.

Last updated: 2026-11-17 (M3 GUI on `feat/m3-gui`).

## Next session

1. **App matrix run on CachyOS (closes M2).** Use each app for about 2 minutes
   while the sampler runs: Discord (GIFs or a call), GitHub Desktop, LibreWolf
   with a 1080p/4K video, mpv/VLC (also with `hwdec=nvdec`), Heroic or Lutris
   idle, Wallpaper Engine, PCSX2, and Crimson Desert, Path of Exile 2, The
   Witcher 3 and Factorio (2 min in game, 1 min in the menu). Nine Sols or
   SIGNALIS in the menu shows the layer 2 session hold on hardware.
2. Close issue #2 and tag `v0.0.2` after the matrix run.
3. M3 is written: the IPC backend is verified on CachyOS and the GUI is
   implemented (`feat/m3-gui`). Next: try the GUI on both desktops
   (checklist in section 3), then the security review of the socket.

## Road to v0.1.0

Each step is its own branch and PR to `develop`. Steps 1 (systemd unit) and
most of 2 (M2) are done; see the history file.

### 2. Close M2 (issue #2): app calibration

- [ ] Check that mpv/VLC with CUDA decoding (`hwdec=nvdec`) does not count as
  compute contention.
- [ ] Run the app compatibility matrix below and calibrate the thresholds
  (45 % / 25 % / 1500 MiB); update `internal/detector/README.md`. A 40-minute
  run on 2026-11-14 covered only LibreWolf (max 22 % SM, 293 MiB) and
  Spectacle (17 %), with no false pause in 1200 ticks.

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
| Discord (Electron) | Ignore | CachyOS | Renderer seen in NVML; filter added, verify on hardware |
| Steam client / steamwebhelper | Ignore | CachyOS | Already ignored |
| Riot Client / League of Legends launcher | Ignore (launcher) / Load (game) | Windows only | Riot Vanguard blocks Linux, so EcoFold never sees LoL on Linux; measure only |
| Epic Games Launcher, Battle.net, EA app (via Proton/Lutris/Heroic) | Ignore | CachyOS | CEF launchers; check process names under Wine |
| Adobe Photoshop | Load | macOS (native, `DarwinCollector`); Windows to measure | No native Linux build |
| Adobe Premiere Pro (export, playback) | Load | macOS (native); Windows to measure | No native Linux build |
| Blender (render) | Load | CachyOS (flatpak) | Verified 2026-11-14 through compute contention (OptiX render paused within 6 s) |
| DaVinci Resolve | Load | CachyOS (native) | Not installed |
| OBS (NVENC recording) | Load | CachyOS | Uses the encoder more than SM; check `enc` vs `sm` |
| Baldur's Gate 3 | Load | CachyOS | Verified 2026-11-13: 55-59 % SM, paused |
| Crimson Desert, Deadlock | Load | CachyOS | Pending |
| Video playback in a browser | Ignore | CachyOS, macOS | Hardware decode must not pause |

How to measure:

- **CachyOS:** run the calibration sampler used on 2026-11-14 (a test-only
  `TestCalibrate` kept outside the repo in `~/.cache/ecofold-verify` on
  CachyOS; it logs every GPU process, its SM, VRAM, ignore/session flags and
  the evaluator's decision every 2 s to `~/ecofold-calibration.csv`), or the
  daemon with `-debug` next to `nvidia-smi pmon -s um -c 30`.
- **macOS:** run the daemon with `-debug`.
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

Follow-up (2026-11-14): thresholds alone cannot catch CUDA work, because the
FAH core starves it. Two design rules cover it instead of per-app tuning:
compute contention (above) and an explicit user pause list for apps that must
always stop folding.

### 3. M3 (issue #3): IPC and GUI

- [x] Upgrade both machines and `go.mod` to Go 1.27.4 (GO-2026-6859) before any
  socket exists. `go.mod` says `go 1.27.4`; the Mac's local toolchain is
  still 1.27.3 and downloads 1.27.4 through `GOTOOLCHAIN=auto`.
- [x] Unix domain socket with restrictive permissions (owner/group only) and a
  minimal protocol: status, force pause/resume, read/update config.
  `internal/ipc` plus the command channel in `internal/daemon`; protocol in
  `internal/ipc/README.md`. Backend only, no GUI yet.
- [x] Verify the IPC backend on CachyOS (2026-11-16, commit `933170e`,
  results in `SECURITY_AUDIT.md` NV-1): tests as the normal user and as
  root, socket `root:ecofold 660` in `root:ecofold 750` under the unit, every
  command against the real NVML and FAH, an unauthorized uid refused with no
  response, and `systemctl stop` removing the socket and restoring 145 W. The
  unit needed `CAP_CHOWN`; the `ExecStartPre` approach did not work.
- [ ] Start the unit without the `ecofold` group (`groupdel ecofold`): it must
  still start, log `socket group not found` and leave the socket `0600`. Only
  a unit test covers this today.
- [ ] IPC changes are not persisted: `set_eco_watts`, `set_pause_processes`
  and the manual pause are lost when the daemon restarts (the flags apply
  again). Decide where a config file lives and who may write it before the
  GUI ships settings.
- [x] The `fah` block of the status was what the daemon saw the last time it
  paused or resumed. The loop now reads the FAH status on its first tick and
  every 5 ticks (10 s), so a pause made in FAH's own UI shows by itself.
- [ ] Pause list on macOS: `set_pause_processes` and `-pause-processes` are
  refused or ignored there (`unsupported`), because `DarwinCollector` reads
  ioreg only. It needs a per-process source before it can be supported.
- [ ] Security review of the socket before merging (new attack surface). The
  daemon runs as root, so a socket bug is a root bug: socket `0660` with an
  `ecofold` group, check `SO_PEERCRED`, cap message size, validate watts
  against the NVML constraints. All four are implemented (NV-1 in
  `SECURITY_AUDIT.md`); the review itself, with the `security-audit` skill in
  the `standard` profile, has not been done.
- [ ] Harden the unit further (`SystemCallFilter=@system-service`,
  `ProtectKernelModules`, `RestrictNamespaces`, `MemoryDenyWriteExecute`) and
  re-check that NVML and cgo still work on CachyOS.
- [x] System tray with state (Folding / Paused by game / Battery) and current
  TDP, plus a control panel. The GUI stays unprivileged. Built with Fyne in
  pure Go instead of Wails: `cmd/ecofold-gui` and `internal/gui`.
- [x] GUI handles the daemon being down (shows "Daemon not running" or
  "Access denied", no crash) and reconnects by itself.
- [ ] **Try the GUI on real desktops.** It was written on macOS. Automated
  tests cover the controller against a real socket and the panel on Fyne's
  software driver; the real binary was only launched on macOS for a few
  seconds (it started, connected to a mock daemon and survived the daemon
  stopping). Nobody has looked at the real window or tray yet.
  - [ ] CachyOS: `make build-gui` with cgo (never built on Linux: the machine
    was unreachable), then `go test -race ./...`.
  - [ ] CachyOS: reinstall the daemon first. The installed build (`933170e`)
    predates `pause_list_supported`, so the GUI would show the pause list as
    unavailable.
  - [ ] KDE Plasma (Wayland): the tray icon appears and changes color with the
    mode, the menu works, and the window's X hides it instead of quitting.
  - [ ] macOS: the menu bar icon and menu, and the control panel with the
    power control hidden and the pause list disabled.
  - [ ] Against the real daemon: pause, resume (also during a game, to see
    the `game_active` message), eco limit and pause list.
- [x] **GUI showed misleading data** (seen on the real window on a MacBook Air
  M4, 2026-11-16). Fixed 2026-11-17 in
  `fix(gui,daemon): report real fah status and accurate macos gpu telemetry`.
  - [x] **1. Real FAH status.** The loop reads `fah.GetStatus` on its first
    pass and every 5 ticks; `daemon.FAHStatus` and `gui.Status.FAH` carry
    `state` and `active_project_id`. An unreachable FAH shows
    "Folding@home: not reachable (is the client running?)", a gray icon and
    the headline "Folding@home Not Reachable" instead of "Folding".
  - [x] **2. No mock GPU data on macOS.** `gpu.Telemetry.Simulated` marks the
    mock and the Linux stub; the daemon never publishes their watts,
    temperature or VRAM. The name (`Apple M4`), utilization and in-use memory
    come from ioreg through `SystemSnapshot`, and the panel shows
    "GPU utilization: X % · In-use memory: Y MiB". Checked against the real
    daemon on the MacBook Air M4.
  - [x] **3. Tests and checks.** `gofmt -l .`, `go vet ./...`,
    `go test -race ./...` and `make build` pass on macOS.
- [ ] GUI follow-ups, none needed to close M3: start at login (`-hidden`
  with an XDG autostart entry / a LaunchAgent), an application icon and
  bundle for macOS, and the Folding@home project details in the panel.

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

- [ ] **R5 CPU-only renders pause folding.** An app with a CUDA context that
  renders on the CPU (Blender Cycles on CPU: ~1140 % CPU, 1487 MiB, GPU at
  0 %) matches the compute-contention rule, so FAH pauses with an idle GPU.
  Accepted for `v0.1.0` (costs folding time only). Fix idea: require a drop of
  the FAH core's own SM (it fell from ~95 % to 49-80 % under the OptiX render
  and does not move under a CPU render); calibrate against its normal 75-98 %
  swing.
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
- [ ] **L2-1** (accepted, see `CHAOS_REPORT_LAYER2.md`): a user process can
  pose as `reaper SteamLaunch`; it only holds a pause its own load caused.

## After v0.1.0

- [ ] Evaluate a minimal privileged helper (`ecofold-powerctl`, started
  through polkit/`pkexec`) so the daemon itself can run unprivileged. Today
  NVML requires uid 0 for power limits.

## Housekeeping

- [ ] Publish the FAH container on `127.0.0.1:7396:7396` instead of
  `0.0.0.0:7396`: today any host on the LAN can pause or reconfigure FAH.
- [ ] CachyOS test machine: the installed `/usr/bin/ecofold-daemon` and its
  unit are a manual build of `feat/m3-ipc-socket` at `933170e`, and the
  `ecofold` group exists with the desktop user in it. Reinstall with
  `sudo bash ~/.cache/ecofold-verify/m3/m3-verify.sh` (it also reruns the IPC
  checks) after daemon changes until the AUR package exists; the AUR package
  must create the group (`sysusers.d`). The legacy `autofold.service` (user unit) is
  disabled. The old local branch `test/m1-nvml-integration` in
  `~/Documents/GitHub/ecofoldGPU` only holds a commit already applied on
  `develop`; delete it with `git branch -D` when convenient.
