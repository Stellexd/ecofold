# macOS Telemetry: Measurement Stack, Schema, and Limitations

This document describes the read-only macOS telemetry collector, the IOKit fields available in the checked-in Apple Silicon fixtures, and the platform limits that shape EcoFold's macOS behavior. These measurements were collected on a MacBook Air M4 (`Mac16,12`) with an Apple M4 chip, macOS `27.2`, a 4,629 mAh design-capacity battery with 110 cycles, and an `AGXAcceleratorG16G` GPU node.

The same schema and key set were validated on a Mac Studio M1 Max (`Mac13,1`) with an `AGXAcceleratorG13X` GPU node and no battery (`has_battery = 0`). On that machine, `PerformanceStatistics` exposed the same 11 keys seen on the M4, desktop idle captures showed up to 1338 MiB of in-use GPU system memory while `Device Utilization %` stayed at 0%, and short desktop compositor spikes reached up to 36% utilization. These are resting-state captures: the current EcoFold thresholds (45% GPU utilization, 25% GPU-utilization gate for memory, 1500 MiB memory) still need calibration under real load (a Metal game or render workload).

## Measurement stack

The collector uses macOS system utilities and the Python 3 standard library; it needs no third-party Python packages and no root privileges.

- `ioreg -a -r -d 1 -c <IOKitClass>` emits an XML property list. `plistlib` parses the output.
- `AppleSmartBattery` provides battery state and power-distribution properties.
- `IOAccelerator` provides GPU utilization and system-memory statistics. On the measured M4, the accelerator node's `IOClass` is `AGXAcceleratorG16G`.
- `sysctl -n vm.loadavg` provides the system load average; the collector stores its 1-minute value as `cpu_load_1m`.
- The profile reads `hw.model` and `machdep.cpu.brand_string` with `sysctl`; it obtains the macOS version through Python's `platform.mac_ver()`.
- The collector does not call `nvidia-smi` or `powermetrics`, and does not invoke `sudo`.

The Go daemon's `DarwinCollector` reads the same `ioreg` classes, plus the thermal pressure level (see "Thermal guard") and, once, the model and core counts through `/usr/sbin/sysctl -n hw.model hw.physicalcpu hw.perflevel0.physicalcpu hw.perflevel1.physicalcpu` (see "Hardware profile and FAH CPU threads"). It reads `IOAccelerator` with `ioreg -a -r -d 2 -l -c IOAccelerator`, which also returns each GPU client's `AppUsage` (see "Per-process view"), and lists processes with `sysctl kern.proc.all` while a pause list is set.

## Per-process view

macOS has no `/proc`, but two unprivileged sources name processes:

- **Running processes:** `sysctl kern.proc.all`, read in Go with `unix.SysctlKinfoProcSlice` (no cgo, no external command). `p_comm` is cut to 16 bytes; `kern.procargs2` gives the full executable path for the user's own processes. The pause list (`-pause-processes`, `set_pause_processes`) uses it and works on macOS. Seen on the M4: `Safari`, `Calculator`, and `NotificationCent` for Notification Center.
- **GPU time per app:** each user client of the accelerator (`AGXDeviceUserClient` on the M4) has `IOUserClientCreator` (`pid 604, WindowServer`) and `AppUsage`, one entry per command queue with `accumulatedGPUTime` in nanoseconds. `ioreg -a` only includes these properties with `-l`. Over 2 s at rest, WindowServer used about 4-6 % and VSCodium Helper 2 % while `Device Utilization %` read 4-16 %. The daemon reports the figures in `SystemSnapshot.GPUApps` and logs them with `-debug`; no threshold uses them yet.

`powermetrics` would give more, but it needs root and is not used.

## Thermal guard

On a fanless MacBook Air M4, running the Folding@home client took the CPU to 105 °C and the GPU to 87 °C. The GPU-utilization and battery rules did not catch it: FAH's own load is not a game.

macOS publishes its thermal pressure on the notify(3) key `com.apple.system.thermalpressurelevel`, readable without root through `/usr/bin/notifyutil -g com.apple.system.thermalpressurelevel` (output: `com.apple.system.thermalpressurelevel 0`). `DarwinCollector` reads it on every tick, with a 1 s timeout and a 4 KiB output cap, into `SystemSnapshot.ThermalLevel`.

| Level | `OSThermalPressureLevel` | `ProcessInfo.ThermalState` | Fanless Mac | Other Macs |
|---|---|---|---|---|
| 0 | nominal | nominal | folds (1 thread) | folds |
| 1 | moderate | fair | pauses (`paused_thermal`) | folds with fewer threads |
| 2 | heavy | serious | pauses | pauses (`paused_thermal`) |
| 3 | trapping | critical | pauses | pauses |
| 4 | sleeping | critical | pauses | pauses |

A level at the profile's pause level (1 on a fanless Mac, 2 otherwise) pauses folding on that tick. The pause lasts until the level has stayed below it for the load cooldown (60 ticks, two minutes), so the Mac is not heated again as soon as it drops a few degrees. A failed read does not fail the tick: the last level is kept for 15 ticks (30 s), then 0 (unknown) is assumed. `force_resume` answers `thermal_pressure` while the pause lasts.

The level is the system's own judgement and has no temperatures behind it that EcoFold can read.

**Measured limitation (2026-11-18):** on the MacBook Air M4, the FAH CPU core (`FahCore_a9`, 4 threads, ~400 % CPU) took the CPU to 105–106 °C within 5–10 s, and the level stayed at `0` (`com.apple.system.thermalstatus` also `0`, `pmset -g therm` recorded no warning). The pressure level follows the system's sustained thermal state, not a die spike, so this guard cannot keep the CPU below 100 °C. It is a backstop only; the CPU thread budget below attacks the heat source. See `docs/INCIDENT_2026-11-18_MACOS_THERMAL.md`.

## Hardware profile and FAH CPU threads

FAH folds on the CPU on Apple Silicon, and its thread count is what heats the chip. `DarwinCollector` reads `hw.model` and the core counts once (1 s timeout, 4 KiB cap, up to 3 tries; a failure never fails a tick) and classifies the Mac:

| Profile | Macs | FAH threads on AC | On battery | At pressure level 1 | Pauses at |
|---|---|---|---|---|---|
| `fanless` | MacBook Air (`MacBookAir*`, `Mac14,2`, `Mac14,15`, `Mac15,12`, `Mac15,13`, `Mac16,12`, `Mac16,13`) and any laptop that is not a known MacBook Pro | 1 | 1 | — | 1 |
| `active_laptop` | MacBook Pro (`MacBookPro*`, known M2–M4 `Mac*` ids) | max(2, min(E-cores, physical/2)) | max(1, min(2, E-cores/2)) | 1 | 2 |
| `desktop` | no battery: Mac mini, Mac Studio, iMac, Mac Pro | max(2, physical − 2) | — | max(2, physical/2) | 2 |

Measured on the MacBook Air M4 (`Mac16,12`, 4 P-cores + 6 E-cores, fanless) with FAH 8.5.6 and `FahCore_a9`:

| FAH `cpus` | CPU temperature |
|---|---|
| 1 | ≤ 70 °C, stable |
| 2 | 90 °C in ~35 s |
| 4 | 106 °C in 5–10 s |

The MacBook Pro and desktop budgets follow the same reasoning (leave the performance cores to the user) but are not measured yet.

The daemon writes the budget to FAH's v8 `cpus` option, the same setting as the Web Control's CPU slider, and treats it like the GPU power limit:

- It reads and keeps the user's value before the first change.
- It applies the budget before every resume (also `force_resume`) and on every folding tick, so a switch to battery or a rise to pressure level 1 reaches a running FAH. A value changed in FAH's own UI while EcoFold runs is put back after the next status refresh (10 s).
- If the budget cannot be applied, the daemon does not resume its own pause.
- On exit (`SIGINT`/`SIGTERM`) it restores the user's value within 2 s. FAH keeps folding with that value: on a fanless Mac the log warns, and `ecofold-daemon -pause-fah` pauses FAH.

Wire format, verified against `fah-client` 8.5.6: `cpus` is read from `groups[""].config.cpus` (top-level `config.cpus` on clients without groups) and set with `{"cmd":"config","time":…,"config":{"groups":{"":{"cpus":N},"<other group>":{}}}}`. FAH merges each group's dict into that group and **deletes every named group missing from `groups`**, so every group of the current snapshot is listed. FAH also drops a command of the same kind whose `time` is not a later second than the previous one; the client moves such a time to the next second. Only the default group is limited: threads assigned to other resource groups are not counted.

Known limits:

- The user's original value lives in memory. If the daemon is killed (`SIGKILL`, crash), FAH keeps the budget and the next start records the budget as the user's value.
- Whether a running work unit picks up a new `cpus` value without a restart was not measured; the daemon applies the budget before resuming, so a work unit it resumes starts with it.

## Emergency stop

`fah-client` is a system service and keeps folding without EcoFold. To pause it without the daemon or the Web Control:

```sh
ecofold-daemon -pause-fah
```

It connects to `ws://127.0.0.1:7396/api/websocket`, pauses every resource group, waits up to 3 s for FAH to confirm and exits (0 on success). It opens no socket and never touches NVML, so it works while a daemon runs; a running daemon sees the pause as the user's and does not resume it.

## Single daemon instance

A daemon whose socket is already served by another daemon (`ipc.ErrAlreadyRunning`) exits with code 1 before its control loop starts. On 2026-11-18 a second daemon kept running without IPC, read the first one's manual-pause marker as an inherited automatic pause and resumed FAH. The marker now also records whether the pause was manual, so a restarted daemon keeps a manual pause until `force_resume`.

## Collector and SQLite database

Run from the repository root:

```sh
python3 scripts/mac_collector.py
```

The script writes `mac_telemetry.db` in its current working directory (`DEFAULT_DB_PATH = "mac_telemetry.db"`). It captures a hardware profile once per run, then samples for 120 seconds at 5-second intervals (normally 24 samples per run). All parameters can be overridden without editing the file:

```sh
python3 scripts/mac_collector.py --duration 60 --interval 2 --db /tmp/studio.db
python3 scripts/mac_collector.py --duration 0     # until Ctrl+C
```

The collector runs on any macOS machine: MacBook models (with battery), and Mac mini, Mac Studio, and Mac Pro (no battery). On battery-less machines every battery column is stored as SQL `NULL`, `has_battery` is `0`, and the script prints a notice instead of inventing values. It exits with a clear error on non-macOS systems. Only the Python 3 standard library is required.

File validation is defensive: a failing `ioreg`/`sysctl` call, a truncated plist, or a changed GPU node class degrades to defaults instead of crashing the run.

| Table | Columns |
|---|---|
| `hardware_profile` | `id`, `captured_at`, `hw_model`, `chip_brand`, `os_version`, `has_battery`, `cycle_count`, `design_capacity`, `raw_max_capacity`, `health_pct`, `gpu_io_class`, `gpu_perf_keys_json` |
| `telemetry_samples` | `id`, `timestamp`, `hw_model`, `ac_connected`, `is_charging`, `batt_pct`, `batt_temp_c`, `voltage_mv`, `amperage_ma`, `system_load_mw`, `battery_power_mw`, `net_battery_flow_mw`, `wall_input_mw`, `time_remaining_min`, `gpu_util_pct`, `gpu_render_util_pct`, `gpu_tiler_util_pct`, `gpu_in_use_mem_mb`, `cpu_load_1m` |

`gpu_in_use_mem_mb` stores mebibytes (MiB), not decimal megabytes (MB); the `_mb` suffix is kept for compatibility with the existing schema. `ensure_columns()` checks each table with `PRAGMA table_info` and adds missing columns using `ALTER TABLE ADD COLUMN`, preserving existing rows when the schema grows. `to_signed64()` reinterprets IOKit unsigned 64-bit representations as two's-complement signed values, which is necessary for negative current and power readings.

### Battery-capacity fields

In the observed dumps, `AppleRawMaxCapacity` and `FullChargeCapacity` are nested under `BatteryData`, not at the property-list root. The collector reads `AppleRawMaxCapacity` (falling back to `NominalChargeCapacity` if needed) and computes:

```text
health_pct = raw_max_capacity / design_capacity * 100
```

For example, a measured raw maximum of 4,449 mAh against a design capacity of 4,629 mAh gives an estimated health of 96.11%. This is an estimate, not a precision health measurement; see [Limitations](#limitations).

## Fixtures and inspection tools

`internal/detector/testdata/` contains sanitized real-device captures:

| Fixture | Capture |
|---|---|
| `m4_air_battery_charging.plist` | `AppleSmartBattery` with AC connected and charging |
| `m4_air_battery_discharging.plist` | `AppleSmartBattery` on battery |
| `m4_air_gpu_idle.plist` | `IOAccelerator`, including the `AGXAcceleratorG16G` node |

The captures were generated with `ioreg -a -r -d 1 -c AppleSmartBattery` and `ioreg -a -r -d 1 -c IOAccelerator`. Check their plist syntax with:

```sh
plutil -lint internal/detector/testdata/*.plist
```

`scripts/inspect_testdata.py` loads all fixtures, recursively flattens nested dictionaries, and prints keys matching battery- or GPU-related fields. It is useful for inspecting the captured values without manually opening the XML plist files:

```sh
python3 scripts/inspect_testdata.py                    # repo fixtures
python3 scripts/inspect_testdata.py ~/dumps/           # any directory of dumps
python3 scripts/inspect_testdata.py --all ~/dump.plist # every key
```

Node type is detected from content (`PerformanceStatistics`, `BatteryData`, `AGX*` `IOClass`), so dumps from other Macs can be inspected regardless of filename. Unreadable files are reported and skipped, and the exit code is non-zero if any file fails.

All device serials (`AdapterDetails.SerialString` and `Serial`) in the fixtures are replaced with `REDACTED_SERIAL`. Never commit unsanitized `ioreg` dumps.

## Electrical-state model

Use `ExternalConnected` to determine AC connection (`OnAC = ExternalConnected`), cross-checked by `PowerDistribution.IPDInputPower > 0` when available. Do not require `ExternalConnected && IsCharging`: `IsCharging` becomes false when the battery is full or optimized charging is holding the charge, even though the Mac remains plugged in.

`IsCharging` (or the sign of `InstantAmperage`) distinguishes active charging from plugged-in maintenance; it is not the AC-presence signal. The resulting states are:

| Condition | State |
|---|---|
| `OnAC && IsCharging` | Charging |
| `OnAC && !IsCharging` | Plugged in (full/maintenance) |
| `!OnAC` | On battery |

`ChargerCount` and `ChargerData` are not reliable AC indicators: they can appear without an adapter. `NotChargingReason = 128` was observed while on battery.

### Power-field semantics

All power values below are in milliwatts (mW); current is in milliamps (mA).

| IOKit key | Meaning and interpretation |
|---|---|
| `BatteryData.BatteryPower` | Primary battery-power measurement. Positive while charging, negative while discharging. Observed values include `+10,330 mW` charging and `−4,098 mW` discharging. |
| `InstantAmperage` (fallback: `Amperage`) | Signed battery current: positive entering the battery, negative leaving it. The two keys agree in the captures; examples are `+898 mA` charging and `−360 mA` discharging. |
| Derived battery-power fallback | If `BatteryData.BatteryPower` is absent, the collector calculates `round((Voltage_mV * InstantAmperage_mA) / 1000.0)` in mW. |
| `PowerTelemetryData.BatteryPower` | Net battery/system flow, calculated as `SystemPowerIn − SystemLoad`; with fresh data it has the same sign as battery power. Because `SystemPowerIn` can be stale, this is stored separately as `net_battery_flow_mw`, not used as the primary battery-power source. |
| `PowerTelemetryData.SystemLoad` | System consumption in mW; positive in the observed captures. |
| `PowerTelemetryData.SystemCurrentIn` | Reported as `0` in all measured captures; do not use it as an input-current signal. |
| `PowerDistribution.IPDInputPower` | Wall/adaptor input power in mW; the most reliable adapter-input measurement in the captures. It is `0` on battery. |

The stale-input behavior is material: shortly after a 30 W adapter was connected, `SystemPowerIn` was observed at just `16 mW`, yielding a misleading net flow. In a stable charging sample, `PowerTelemetryData.BatteryPower` was `23,537 mW`, `PowerTelemetryData.SystemLoad` was `4,674 mW`, and `PowerTelemetryData.SystemPowerIn` was `28,211 mW`. With an adapter attached, `AdapterDetails` included `Name = "30W USB-C Power Adapter"`, `Watts = 30`, `AdapterVoltage = 20000 mV`, `Current = 1490 mA`, and `IsWireless = false`.

## Example measurement snapshots

These ranges illustrate captured telemetry; they are not fixed device guarantees.

| State | Battery | Voltage | Current | System load | Battery power | GPU utilization / in-use memory | CPU load (1m) |
|---|---:|---:|---:|---:|---:|---:|---:|
| Discharging | 41–42% | 11,351–11,415 mV | −390 to −962 mA | 4,451–10,919 mW | −4,451 to −10,919 mW | 6–19%; 430–671 MiB (704 MB) | 2.3–2.55 |
| Charging (30 W adapter) | — | 11,817 mV | +1,988 mA | — | +23,492 mW | — | — |

In that charging snapshot, `net_battery_flow_mw` was `+23,537 mW`, wall input was `29,800 mW`, and `time_remaining_min` was 153. A separate adapter measurement of `20,000 mV × 1,490 mA` corresponds to approximately 29,800 mW at the wall-input field.

## Limitations

- **Battery temperature is unavailable without changing the privilege model.** The M4's `AppleSmartBattery` node does not expose `Temperature`; `DeadBatteryBootData.GeneralPayload.AverageBattSkinTemp` and `AverageBattVirtualTemp` are always `0` in the observed captures. Unprivileged `pmset -g therm` reports historical thermal events rather than live sensors and provides no readings on a healthy device. `powermetrics` requires root, while the GUI must run unprivileged. The collector therefore stores `batt_temp_c` as SQL `NULL`; temperature must not be used for macOS detection. The thermal guard uses the system's thermal pressure level instead.
- **`TimeRemaining` uses a sentinel.** `65535` (`0xFFFF`) means unknown and is stored as `NULL`; zero is also left `NULL`. A discharge capture reported 219 minutes, while charging reported `65535`.
- **Apple GPU telemetry is partial.** `AGXAcceleratorG16G` exposes `PerformanceStatistics`, including `Device Utilization %`, `Renderer Utilization %`, `Tiler Utilization %`, `In use system memory`, and `Alloc system memory`. The collector stores the in-use system-memory value (converted from bytes to MiB), not the allocation value. The measurements do not expose GPU temperature, power, or total VRAM. Idle utilization was already about 24%, so absolute GPU-utilization thresholds risk false positives; use a baseline/delta or combine GPU data with process information. Per-app GPU time is now available (see "Per-process view") but is not calibrated.
- **There is no Apple Silicon TDP control through this provider.** The SoC firmware manages the GPU's power behavior. On macOS, `MockProvider` reports `SupportsPowerLimit = false`; GPU power limiting is unsupported.
- **Battery health is noisy.** `AppleRawMaxCapacity` can recalibrate over minutes (4,493 to 4,516 mAh was observed). Treat the computed percentage as an estimate.
- **Go provider units are intentionally coarser than raw measurements.** `gpu.Telemetry` exposes integer watts and MiB. `mwToW` uses integer division and truncates (for example, 144.9 W becomes 144 W). `NvidiaProvider` selects NVIDIA device index 0; it does not enumerate multiple GPUs.

## Quick reference

| Key / field | Where | Use |
|---|---|---|
| `ExternalConnected` | `AppleSmartBattery` | AC-state signal (`OnAC`); cross-check `IPDInputPower > 0`. |
| `IsCharging` | `AppleSmartBattery` | Charging versus plugged-in maintenance; not AC presence. |
| `BatteryData.BatteryPower` | `AppleSmartBattery` → `BatteryData` | Primary signed battery power, mW. |
| `AppleRawMaxCapacity` / `FullChargeCapacity` | `AppleSmartBattery` → `BatteryData` | Raw maximum and full-charge capacity in mAh; health estimate uses raw maximum / design capacity. |
| `InstantAmperage` / `Amperage` | `AppleSmartBattery` | Signed battery current, mA; fallback input for derived power. |
| `PowerTelemetryData.SystemLoad` | `AppleSmartBattery` → `PowerTelemetryData` | Positive system load, mW. |
| `PowerTelemetryData.SystemPowerIn` | `AppleSmartBattery` → `PowerTelemetryData` | Input estimate used by net flow; may be stale after adapter connection. |
| `PowerTelemetryData.BatteryPower` | `AppleSmartBattery` → `PowerTelemetryData` | `SystemPowerIn − SystemLoad`; potentially stale, stored separately. |
| `PowerDistribution.IPDInputPower` | `AppleSmartBattery` → `PowerDistribution` | Wall input power, mW; zero on battery. |
| `TimeRemaining` | `AppleSmartBattery` | Minutes when valid; `65535` means unknown (`NULL`). |
| `IOClass` | `IOAccelerator` | GPU node class; `AGXAcceleratorG16G` on measured M4. |
| `PerformanceStatistics.* Utilization %` | `IOAccelerator` → `PerformanceStatistics` | Device, renderer, and tiler utilization percentages. |
| `In use system memory` | `IOAccelerator` → `PerformanceStatistics` | GPU in-use system memory in bytes; collector stores MiB. |
| `com.apple.system.thermalpressurelevel` | `notifyutil -g` | Thermal pressure 0–4; 1 or more pauses a fanless Mac, 2 or more any other. |
| `hw.model`, `hw.physicalcpu`, `hw.perflevel0/1.physicalcpu` | `sysctl -n` | Hardware profile and FAH CPU thread budget. |
| `Alloc system memory` | `IOAccelerator` → `PerformanceStatistics` | GPU-allocated system memory in bytes; exposed by IOKit but not stored by the collector. |

## Repeat the measurements

From the repository root, capture a battery dump with and without the charger and a GPU dump:

```sh
ioreg -a -r -d 1 -c AppleSmartBattery > internal/detector/testdata/<name>.plist
ioreg -a -r -d 1 -c IOAccelerator > internal/detector/testdata/<name>.plist
plutil -lint internal/detector/testdata/*.plist
python3 scripts/inspect_testdata.py
python3 scripts/mac_collector.py
```

Use descriptive filenames for the charging, discharging, and GPU states. Sanitize all serials before committing any new dump.
