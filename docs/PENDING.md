# Pending Work

Open work only, ordered by priority. Tick each box in the PR that completes
it, then move it to `docs/archive/PENDING_HISTORY.md` when the section is
done. Finished work and its context (M1 review, chaos passes, daemon loop,
systemd verification, layer 2, compute contention):
`docs/archive/PENDING_HISTORY.md` and the reports in `docs/`.

Last updated: 2026-11-18 (macOS pause list and per-app GPU time).

## Next session

0. **macOS thermal incident (2026-11-18): fixed in code, not yet seen live
   under load.** Single instance, `-pause-fah`, the manual-pause marker, the
   pause enforcement (INC-3) and the FAH CPU thread budget are implemented and
   tested with fakes; `SetCPUs` was checked against the real `fah-client`
   8.5.6 with FAH paused. Next: run the daemon on the M4 Air with FAH folding
   and watch the temperature with the budget of 1 thread (boxes under
   section 3). Until then, do not leave FAH folding on the M4 unattended.
1. **App matrix run on CachyOS (closes M2).** Use each app for about 2 minutes
   while the sampler runs: Discord (GIFs or a call), GitHub Desktop, LibreWolf
   with a 1080p/4K video, mpv/VLC (also with `hwdec=nvdec`), Heroic or Lutris
   idle, Wallpaper Engine, PCSX2, and Crimson Desert, Path of Exile 2, The
   Witcher 3 and Factorio (2 min in game, 1 min in the menu). Nine Sols or
   SIGNALIS in the menu shows the layer 2 session hold on hardware.
2. Close issue #2 and tag `v0.0.2` after the matrix run.
3. M3 is written: the IPC backend is verified on CachyOS, the GUI is
   implemented and the pause list works on macOS. What is left is hardware
   checks (lists below).

### To close M2 (issue #2)

- [ ] App matrix run on CachyOS (item 1), including mpv/VLC with
  `hwdec=nvdec`, and calibrate the thresholds from it.
- [ ] Close issue #2 and tag `v0.0.2`.

### To close M3 (issue #3)

- [ ] Try the GUI on the CachyOS desktop (checklist in section 3).
- [ ] Run the M4 Air with FAH folding on the 1-thread CPU budget and watch
  the temperature (item 0).
- [ ] macOS pause list on AC: folding resumes 2 minutes after the matching
  app quits. Only checked on battery so far.
- [ ] Start the unit without the `ecofold` group: it must start, log
  `socket group not found` and leave the socket `0600`.
- [ ] Security review of the socket (`security-audit` skill, `standard`
  profile).

Not needed to close M3: decide where the config is persisted (eco watts and
the pause list are lost on restart), harden the systemd unit, and calibrate
the macOS per-app GPU time (`GPUApps`) under a real Metal game or render.

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
- [ ] IPC changes are not persisted: `set_eco_watts` and
  `set_pause_processes` are lost when the daemon restarts (the flags apply
  again); the manual pause is kept through the marker since 2026-11-18. Decide where a config file lives and who may write it before the
  GUI ships settings.
- [x] The `fah` block of the status was what the daemon saw the last time it
  paused or resumed. The loop now reads the FAH status on its first tick and
  every 5 ticks (10 s), so a pause made in FAH's own UI shows by itself.
- [x] Pause list on macOS (2026-11-18): `DarwinCollector` lists processes with
  `sysctl kern.proc.all` (no root, no cgo) and resolves 16-byte `p_comm`
  names through `kern.procargs2`. Checked on the M4 Air with a test daemon
  and the mock FAH, on battery: `set_pause_processes` matched `safari`,
  `calculator` and `notificationcenter` (from `NotificationCent`), the mode
  went to `paused_load` 3 ticks later and back to `paused_battery` on the
  first tick after Calculator quit.
  - [ ] Not yet seen on AC: the resume of folding after the 2-minute cooldown
    once the matching app quits. On battery the battery pause takes over, so
    the cooldown never shows.
- [ ] macOS per-app GPU time (`SystemSnapshot.GPUApps`, from `AppUsage` in
  `ioreg -d 2 -l -c IOAccelerator`): implemented and logged with `-debug`
  (`msg="gpu apps"`), not used by the evaluator. Calibrate it under a real
  Metal game or render before any rule reads it; see
  `internal/detector/README.md` for why it does not go into `GPUProcesses`.
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
  - [x] CachyOS: `make build` with cgo and `go test -race ./...` pass
    (2026-11-17, commit `19a276c`, Go 1.27.4). Run as the normal user for a
    few seconds, the new daemon reported the real FAH state (`folding`,
    project 18264) without pausing, and the NVML telemetry. `ecofold-gui
    -hidden` started in the Plasma Wayland session, registered its
    StatusNotifierItem and exited cleanly on SIGINT. Nobody looked at it:
    the visual checks below are still open. It used about 250 MiB of RSS
    while hidden; measure it properly against the "minimal footprint" rule.
  - [x] CachyOS: daemon reinstalled from `19a276c` (2026-11-17). Active,
    socket `0660` in a `0750` directory, 123 W while folding, and the status
    carries `state`, `active_project_id` and `pause_list_supported`. The
    control panel was seen on Plasma: green dot, "running (project #18264)",
    the NVML usage line, the eco slider (range 123-145 W) and the pause list
    enabled.
  - [x] KDE Plasma (Wayland), 2026-11-17: the tray icon appears, the menu
    works, the window's X hides it instead of quitting, and Pause Folding
    turns the icon gray, pauses FAH and restores 145 W.
  - [ ] KDE Plasma: the icon turns blue during a game.
  - [ ] macOS: the menu bar icon and menu, and the control panel with the
    power control hidden and the pause list disabled.
  - [ ] Against the real daemon: resume (also during a game, to see the
    `game_active` message), the eco limit applied while folding and the
    pause list. On 2026-11-17 the eco limit was set to 130 W during a manual
    pause: the daemon accepted it and correctly kept 145 W, so the applied
    cap was not seen.
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
- [x] **Control panel redesign and project metadata** (2026-11-17). The GUI
  fetches `api.foldingathome.org/project/<id>` (3 s timeout, cached by id, a
  failure is retried after a minute and never marks the daemon offline) and
  the "Scientific Impact & Compute Engine" card shows the cause, institution
  and a link to the stats page. The GPU figures are a 3-column grid. The
  BOINC engines in the selector only announce v0.2. The "Community
  Leaderboard" opt-in is stored in the local preferences and sends nothing:
  `ecofold-web` does not exist yet. Only seen on Fyne's software driver;
  look at it on Plasma and macOS.
- [x] **Header contradicted the FAH line** (seen on macOS, 2026-11-17:
  "Folding (on battery)" in green over "paused by EcoFold"). While the mode
  allows folding the header now follows FAH: "Resuming Folding…" for the
  daemon's own pause, "Paused in Folding@home" for the user's, "Idle (Waiting
  for Work Unit)" without a work unit; all gray. During the startup hold
  (`startup_hold` in the status) it says "Starting Folding…" instead.
- [x] **Contribution counters.** The daemon publishes `folding_seconds` and
  `projects_contributed` for its current run (memory only), and the
  leaderboard card shows "Session compute donated: Xh Ym · Projects: N". The
  GUI keeps a random
  `client_uuid` in its preferences. Still nothing is sent anywhere.
  - [ ] Persist the totals across daemon restarts if the leaderboard needs
    lifetime figures.
- [ ] **macOS thermal protection: the pressure guard is not enough**
  (2026-11-18). `DarwinCollector` reads
  `com.apple.system.thermalpressurelevel` and pauses at 2+ as
  `paused_thermal` (`b476b77`). Live on the M4 Air the level stayed at 0
  while the CPU was at 106 °C, so the guard never fired. See
  `docs/INCIDENT_2026-11-18_MACOS_THERMAL.md`.
  - [x] Limit FAH's CPU threads on macOS through the v8 `cpus` option
    (record and restore the user's value), with values measured on the M4:
    1 thread kept it at ≤ 70 °C, 2 reached 90 °C in ~35 s, 4 reached 106 °C
    in 5–10 s. Profiles `fanless` / `active_laptop` / `desktop` from
    `hw.model` (`internal/detector/darwin_profile.go`); budget applied before
    every resume and restored on exit (`docs/MACOS_TELEMETRY.md`).
  - [ ] Run the daemon on the M4 Air with FAH folding and confirm it sets
    `cpus` 1 and the CPU stays at or below 70 °C; restart the daemon and
    check the user's value comes back.
  - [ ] Measure the MacBook Pro and desktop budgets (Mac Studio M1 Max is
    available) under FAH load.
  - [ ] Persist the user's original `cpus` so a killed daemon does not record
    the budget as the user's value on the next start.
  - [x] Pause a fanless Mac at pressure level 1; level 2 stays the rule for
    the other profiles.
  - [ ] Trace the pressure level under sustained FAH load to see if and when
    it reaches 1 or 2.
  - [x] Document an emergency stop that works without the daemon:
    `ecofold-daemon -pause-fah` (`docs/MACOS_TELEMETRY.md`).
- [x] **Single daemon instance** (incident 2026-11-18). A second daemon whose
  socket is taken (`ipc.ErrAlreadyRunning`) now exits with code 1 before its
  loop starts, without touching the GPU, FAH or the marker. Other socket
  errors still leave the daemon running without IPC.
- [x] **Manual pause survives a restart** (incident 2026-11-18, 3.5). The
  marker holds `auto` or `manual`; a manual pause restarts in
  `paused_manual` and is never resumed by the startup hold.
- [ ] GUI follow-ups, none needed to close M3: start at login (`-hidden`
  with an XDG autostart entry / a LaunchAgent), and an application icon and
  bundle for macOS.

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
- [ ] Folding@home v8 is installed separately, never bundled: declare
  `optdepends=('foldingathome' 'docker')` in the AUR `PKGBUILD` and say so in
  the `caveats` of the Homebrew tap. The GUI's "Folding@home Setup" card only
  links to the official download and offers a command to copy.
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

- [x] **INC-3 The daemon does not enforce its own pause** (incident
  2026-11-18). The FAH refresh (every 10 s) now pauses FAH again on the same
  tick during `paused_load`, `paused_battery` and `paused_thermal`. During
  `paused_manual`, or any other mode, a resume made outside EcoFold is the
  user's (CH-10): the override, `paused_by_us` and the marker are cleared and
  the status reports what FAH does.

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
