# Incident: MacBook Air M4 at 106 °C while EcoFold showed a pause

Date: 2026-11-18, about 00:59–01:06 (-03:00)

## 1. Summary

The Folding@home CPU core ran on a fanless MacBook Air M4 at about 400 % CPU
and took the CPU to 105–106 °C within 5–10 seconds of starting. EcoFold did
not stop it, for two independent reasons:

1. **The thermal guard never fired.** It pauses at
   `com.apple.system.thermalpressurelevel >= 2`, and macOS kept reporting
   level 0 (nominal) at 106 °C.
2. **Two daemons controlled FAH at the same time.** A daemon from an older
   build still owned the GUI socket and showed `paused_manual`, while a newer
   daemon, running without IPC and invisible to the GUI, resumed FAH.

Folding was stopped by hand by sending a pause to the FAH client's local API.
Stopping the two daemons did not stop it, because the FAH client is a system
service that keeps folding without EcoFold.

| Item | Value |
| --- | --- |
| Machine | MacBook Air M4 (`Apple M4`, 4 P-cores + 6 E-cores, fanless), macOS 27.2 |
| Power | On battery, 74 % |
| FAH | `fah-client` 8.5.6 as `_fahclient` (pid 832, a system service), core `FahCore_a9` (GROMACS, `cpu-neon`, `-np 4`), project 18803 |
| EcoFold | `feat/m3-gui` at `42230f6`, binaries built 00:47 |
| Workload | CPU only. The GPU was at 6 % utilization. |

## 2. Timeline

| Time | Event | Source |
| --- | --- | --- |
| 2026-11-17 01:26:31 | Daemon A (pid 58797) starts from `./bin/ecofold-daemon`, launched by an earlier Claude Code shell. It is an older build: no thermal guard, no `folding_seconds`. It creates the socket `~/Library/Caches/ecofold/ecofold.sock`. | `ps` (start time, parent command), socket mtime, `lsof` |
| 2026-11-17 / 18 | At some point the user pauses from the GUI: daemon A is in `paused_manual` with `paused_by_us: true` and the pause marker on disk. | Status of daemon A at 01:02:25 |
| 00:47 | `make build` with the thermal guard (`b476b77`…`42230f6`). | `bin/` mtimes |
| 00:59:08 | Daemon B (pid 92727) starts from the new binary. The socket is held by daemon A: `ipc.Listen` fails and daemon B **keeps running without IPC** (by design, see 3.2). | `ps`, `lsof` (only daemon A has the socket), `cmd/ecofold-daemon/ipc.go` |
| 00:59:08 | Daemon B finds the pause marker of daemon A in the same runtime dir and takes the pause as its own inherited pause, with the 120 s startup hold. | Code: both use `~/Library/Caches/ecofold`; `daemon.start` sets `resumeHold` when the marker is present |
| 00:59:18 | The GUI (pid 92921) starts and connects to the socket, which is daemon A's. | `ps`, `lsof` |
| ~01:01:08 | The startup hold of daemon B ends (60 ticks × 2 s after 00:59:08). Its evaluator says `folding_eco` (thermal level 0, battery 74 % ≥ 50 %), so it **resumes FAH**. | Inferred from the timing; daemon B's log went to its terminal and was not captured |
| 01:01:09 | `FahCore_a9` starts. | `ps` start time |
| ~01:01:15 | CPU at 105 °C, 5–10 s after folding started. | User report |
| 01:02:25 | Daemon A's status: `mode: paused_manual`, `paused_by_us: true`, while its own FAH reading says `paused: false`, `state: folding`, project 18803. The GUI shows this status. | `status` over the socket |
| ~01:03 | `notifyutil -g com.apple.system.thermalpressurelevel` reads `0` three times, and `com.apple.system.thermalstatus` reads `0`. `pmset -g therm`: "No thermal warning level has been recorded". The user reports 106 °C. | Commands run during the incident |
| ~01:04 | `kill` stops both daemons. `FahCore_a9` keeps running at 395.6 % CPU. | `ps` |
| ~01:06 | A pause is sent through `fah.ClientV8.Pause` (`ws://127.0.0.1:7396/api/websocket`, no root). `FahCore_a9` exits within 4 s. FAH stays paused. | Command output |

## 3. Root causes

### 3.1 The thermal guard reads the wrong signal for this failure

`b476b77` pauses folding when the macOS thermal pressure level is 2
(heavy, which `ProcessInfo` calls "serious") or more. It assumed that the level
rises with CPU temperature. On the M4 Air it did not: level 0 at 106 °C.

What the incident shows:

- Apple Silicon lets the CPU die run up to roughly 105–110 °C under load and
  throttles on its own. The pressure level tracks the whole system's thermal
  state (chassis and skin temperature over minutes), not a die spike.
- A pause at level 2 therefore comes, if it comes at all, minutes after the
  CPU is already at 105 °C. It cannot keep the CPU below 100 °C.
- The guard was written and tested against level values only, without a live
  reading under FAH load. `docs/PENDING.md` had this check as an open item.

There is no die temperature that EcoFold can read on Apple Silicon within
the current rules: `AppleSmartBattery` has no temperature, `powermetrics`
needs root and shows only the pressure level on Apple Silicon, and the
sensors behind `IOHIDEventSystemClient` need cgo and a private API, which
`go-hardware-arch` forbids.

The guard is not wrong, just not enough on its own. It stays as a backstop for
sustained heat, but it is not the protection the M4 Air needs.

### 3.2 A second daemon runs without IPC and fights the first

`startIPC` logs a socket error and lets the daemon keep running, so capping
and restoring the GPU does not depend on the GUI. That makes sense for a
broken socket path, but not for `ipc.ErrAlreadyRunning`: another daemon is
alive, and both control loops then drive the same FAH client:

- Both use the same pause marker (`~/Library/Caches/ecofold`), so the new
  daemon took the old daemon's manual pause as an inherited daemon pause and
  resumed it after the startup hold.
- The daemon without IPC cannot be seen or controlled: the GUI showed the
  old daemon's "Paused (Manual)" while FAH folded.

On Linux the systemd unit makes a second instance unlikely. On macOS, while
the daemon is started by hand, it happened on the first try.

### 3.3 The daemon does not enforce its own pause

Daemon A was in `paused_manual` with `paused_by_us: true`, read FAH as
folding, and did nothing. `Daemon.apply` acts on transitions only: it pauses
FAH when the decision goes into a pause and does not pause again if FAH is
resumed behind its back. With a single daemon that only happens when the user
resumes from FAH's own UI, which the daemon must respect (CH-10). Here it was
another EcoFold daemon. The status then says something false
(`paused_by_us: true` over `state: folding`), and the GUI repeats it.

Not reproduced in a test yet. The fix must not break CH-10: a resume made by
the user in FAH has to stay the user's decision.

### 3.4 Stopping EcoFold does not stop folding

`fah-client` is a system service (`_fahclient`, started at boot). EcoFold only
sends pause and resume. Stopping the daemon leaves FAH as it is, by design,
but there was no quick documented way to pause FAH without the daemon or
the FAH web UI.

## 4. What was wrong in the delivery

- The thermal guard was presented as the fix for 105 °C without being run
  once under real FAH load on the M4. The tests checked the logic against
  level values that the hardware never produced.
- The handover listed "check on the M4 that FAH reaches level 2" as pending,
  but did not say that, until then, the Mac had no thermal protection.

## 5. Proposed fixes

Each item has a box in `docs/PENDING.md`.

1. **Single instance.** `ipc.ErrAlreadyRunning` is fatal at startup: the
   daemon exits with an error. Other socket errors stay non-fatal.
2. **Limit FAH's CPU use on macOS.** Set the FAH v8 `cpus` option over the
   same WebSocket (for example 2 instead of 4 threads, and fewer on battery),
   and restore the user's value on exit. This attacks the heat source. It
   needs a live measurement on the M4 to choose the values, and it changes the
   user's FAH settings, which must be recorded and restored like the GPU limit.
3. **Enforce the daemon's own pause** (3.3) without resuming or overriding a
   user's resume in FAH.
4. **Keep the pressure guard** as a backstop. Possibly add a rule for level 1
   held for minutes, after measuring how the levels evolve under load.
5. **Document an emergency stop** that works without the daemon: pause in
   the FAH web UI, or `fah.ClientV8.Pause` through a small `ecofold-ctl` or a
   `-pause-fah` flag on the daemon.

Rejected for now: reading the die temperature through cgo
(`IOHIDEventSystemClient`, a private API). It breaks the no-cgo rule of
`go-hardware-arch` and could change with any macOS update.

## 6. Evidence still missing

- Daemon B's log (it went to the terminal that started it). It would confirm
  the resume at ~01:01:08 and the evaluator's decision.
- A pressure level trace under sustained FAH load on the M4, to learn
  whether it ever reaches 1 or 2, and after how long.
- When and how daemon A entered `paused_manual` (the GUI, or `force_pause`
  from another client).
