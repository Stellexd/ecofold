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

The stale-input behavior is material: shortly after a 30 W adapter was connected, `SystemPowerIn` was observed at just `16 mW`, yielding a misleading net flow. In a stable charging sample, `BatteryPower` was `23,537 mW`, `SystemLoad` was `4,674 mW`, and `SystemPowerIn` was `28,211 mW`. With an adapter attached, `AdapterDetails` included `Name = "30W USB-C Power Adapter"`, `Watts = 30`, `AdapterVoltage = 20000 mV`, `Current = 1490 mA`, and `IsWireless = false`.

## Example measurement snapshots

These ranges illustrate captured telemetry; they are not fixed device guarantees.

| State | Battery | Voltage | Current | System load | Battery power | GPU utilization / in-use memory | CPU load (1m) |
|---|---:|---:|---:|---:|---:|---:|---:|
| Discharging | 41–42% | 11,351–11,415 mV | −390 to −962 mA | 4,451–10,919 mW | −4,451 to −10,919 mW | 6–19%; 430–671 MiB (704 MB) | 2.3–2.55 |
| Charging (30 W adapter) | — | 11,817 mV | +1,988 mA | — | +23,492 mW | — | — |

In that charging snapshot, `net_battery_flow_mw` was `+23,537 mW`, wall input was `29,800 mW`, and `time_remaining_min` was 153. A separate adapter measurement of `20,000 mV × 1,490 mA` corresponds to approximately 29,800 mW at the wall-input field.

## Limitations

- **Battery temperature is unavailable without changing the privilege model.** The M4's `AppleSmartBattery` node does not expose `Temperature`; `DeadBatteryBootData.GeneralPayload.AverageBattSkinTemp` and `AverageBattVirtualTemp` are always `0` in the observed captures. Unprivileged `pmset -g therm` reports historical thermal events rather than live sensors and provides no readings on a healthy device. `powermetrics` requires root, while the GUI must run unprivileged. The collector therefore stores `batt_temp_c` as SQL `NULL`; temperature must not be used for macOS detection.
- **`TimeRemaining` uses a sentinel.** `65535` (`0xFFFF`) means unknown and is stored as `NULL`; zero is also left `NULL`. A discharge capture reported 219 minutes, while charging reported `65535`.
- **Apple GPU telemetry is partial.** `AGXAcceleratorG16G` exposes `PerformanceStatistics`, including `Device Utilization %`, `Renderer Utilization %`, `Tiler Utilization %`, `In use system memory`, and `Alloc system memory`. The collector stores the in-use system-memory value (converted from bytes to MiB), not the allocation value. The measurements do not expose GPU temperature, power, or total VRAM. Idle utilization was already about 24%, so absolute GPU-utilization thresholds risk false positives; use a baseline/delta or combine GPU data with process information.
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
