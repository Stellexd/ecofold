# internal/detector — design rules (macOS and Linux)

Notes derived from the real fixtures in `testdata/` (MacBook Air M4, macOS 27.2).

## Electrical-state model

Do not use `ExternalConnected && IsCharging` as the AC condition. At 100% charge
or when optimized charging reaches its limit, `IsCharging` becomes `false` even
though the computer is still plugged into the wall.

```
OnAC        = ExternalConnected              # corroborated by IPDInputPower > 0
IsCharging  = IsCharging (or Amperage > 0)    # distinguishes Charging vs Plugged in only
```

States:
- `OnAC && IsCharging` → **Charging**
- `OnAC && !IsCharging` → **Plugged in** (battery full / maintenance)
- `!OnAC` → **Discharging**

## Power semantics (verified in fixtures)

| Source | Meaning | Sign |
|---|---|---|
| `BatteryData.BatteryPower` | Battery power in mW | `+` charging, `−` discharging |
| `InstantAmperage` | Current in mA | `+` flowing in, `−` flowing out |
| `PowerTelemetryData.BatteryPower` | Derived net flow: `SystemPowerIn − SystemLoad` (same sign as battery power, but inherits error if `SystemPowerIn` is stale) | `+` charging, `−` discharging |
| `PowerTelemetryData.SystemPowerIn` | SMC-estimated input power (mW) | `0` without an adapter; may be stale/near zero after connection |
| `PowerTelemetryData.SystemLoad` | System consumption (mW, positive) | always `+` |
| `PowerDistribution.IPDInputPower` | Wall input power (mW), the most reliable with an adapter | `0` without an adapter (~29800 with a 30 W adapter) |

`PowerTelemetryData.SystemCurrentIn` is always `0`: do not use it.

Battery-power fallback: `round((Voltage_mV * InstantAmperage_mA) / 1000.0)`.

## Sentinels and unsupported fields

- `TimeRemaining == 65535` (`0xFFFF`) → unknown, map to `NULL`.
- `Temperature` does not exist in the M4's `AppleSmartBattery`;
  `AverageBattSkinTemp` / `AverageBattVirtualTemp` are always `0`.
  `pmset -g therm` only reports historical events and `powermetrics` requires root:
  temperature is **unavailable** to the unprivileged GUI process.
- `ChargerCount` / `ChargerData` are not reliable AC signals (they appear without an adapter).
- Idle GPU utilization ≈ 24%: do not use idle-level GPU thresholds.
- `AppleRawMaxCapacity` recalibrates over minutes (4493 → 4516 mAh): battery
  health is a noisy estimate.

## Load-detection thresholds

The evaluator (`detector.Evaluator`) selects `StatePausedLoad` when sustained
game workload is detected. The current rule is:

```text
high_load := GPUDeviceUtilPct >= GPUUtilThresholdPct (45% by default)
          || (GPUInUseMemMiB >= GPUMemThresholdMiB (1500 MiB by default)
              && GPUDeviceUtilPct >= GPUMemRequiresUtilPct (25% by default))
```

`GPUMemRequiresUtilPct` is the additional gate: it requires a GPU-memory spike to
be accompanied by minimum utilization. This avoids false positives on unified-
memory systems (Apple Silicon), where the desktop can have up to 1338 MiB
reserved while GPU utilization is 0%. Memory alone is not a game signal.

On Linux the rule is applied **per process** instead (see
[Linux collector](#linux-collector)): when `SystemSnapshot.GPUProcesses` is
non-nil, the device-wide fields are ignored and a process counts as load only
if its own SM utilization and its own VRAM meet the thresholds. Memory from one
process is never combined with another process's utilization, and processes
with `SMUtilValid=false` never count.

Default values (`DefaultConfig`):

| Field | Value | Behavior |
|---|---|---|
| `GPUUtilThresholdPct` | 45 | General utilization that directly triggers a pause. |
| `GPUMemThresholdMiB` | 1500 | In-use memory that triggers a pause together with `GPUMemRequiresUtilPct`. |
| `GPUMemRequiresUtilPct` | 25 | Minimum utilization that must accompany high memory use. |
| `SustainedTicksRequired` | 3 | Consecutive high-load ticks before declaring a pause. |
| `CooldownTicksRequired` | 60 | Consecutive load-free ticks before folding resumes after a load pause (2 min at a 2 s tick). |

The `NewEvaluator` constructor clamps invalid or overly aggressive values to
defaults; for example, `GPUUtilThresholdPct <= 25` is forced to 45% so that the
~24% idle level is not mistaken for a game. The clamps are: `GPUUtilThresholdPct
<= 25` or `> 100` → 45, `GPUMemThresholdMiB <= 0` → 1500,
`SustainedTicksRequired <= 0` or `> 60` → 3, `GPUMemRequiresUtilPct < 25` or
`> GPUUtilThresholdPct` → 25, `MinBatteryPct < 0` or `> 100` → 50, and
`CooldownTicksRequired < 60` → 60 or `> 90` → 90.

### Cooldown (hysteresis)

Once `StatePausedLoad` is declared, the evaluator latches it and keeps returning
it until `CooldownTicksRequired` consecutive load-free ticks have elapsed; any
load tick restarts the count. The cooldown cannot be disabled, so it always
lasts two to three minutes. It never delays `StatePausedBattery`: if the load is
gone but the battery demands a pause during the cooldown, `StatePausedBattery`
is returned.

`AllowBatteryFolding=false` in `Config{}` is intentional and conservative: a
zero-value `Config{}` never folds on battery. Callers that want `true` must start
from `DefaultConfig()`.

## Linux collector

`LinuxCollector` (`linux_collector.go`) builds a `SystemSnapshot` from
`gpu.Provider` and never calls NVML directly, so it also builds with the
no-CGO stub. For every graphics process it reads `/proc/<pid>/comm` and
`basename(argv[0])` from `/proc/<pid>/cmdline` and drops:

- names in `DefaultIgnoredProcesses` (case-insensitive, trailing `*` is a
  prefix match): the Folding@home core and client, desktop compositors, Steam,
  and overlays such as MangoHud. `gamescope` is deliberately **not** ignored
  because games can run inside it;
- browser GPU/renderer helpers (`--type=gpu-process`, `--type=renderer`,
  Firefox `-contentproc`);
- PIDs that vanished between the NVML query and the `/proc` read.

Unreadable identity (permission denied, empty name) is fail-closed: the process
is kept and may count as load. The snapshot reports no battery and leaves
`GPUDeviceUtilPct`/`GPUInUseMemMiB` at zero, because those aggregates include
Folding@home. `Collect` returns an error, instead of an empty snapshot, when
the provider fails or when NVML cannot attribute utilization to any process.
The caller must then keep its previous decision.

## Known limitations / open items

- The thresholds (45% / 25% / 1500 MiB) have not yet been calibrated under real load.
- Linux GPUs/drivers without per-process utilization cannot detect games yet
  (`M2-BLOCK` in `docs/PENDING.md`).
- The Proton/Wine process watcher (detection layer 2) is not implemented yet.
- The battery-less desktop fixture is synthetic (there is no real Mac Studio dump).
- The `OnAC` semantics (`ExternalConnected || IPDInputPower>0`) still need to be
  confirmed with stale telemetry.

## Fixture privacy

Every serial (`AdapterDetails.SerialString`, `Serial`) is replaced with
`REDACTED_SERIAL`. Verify with `plutil -lint internal/detector/testdata/*.plist`
and do not commit unsanitized dumps.
