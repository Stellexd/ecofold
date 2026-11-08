# internal/detector — macOS design rules

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
- Idle GPU utilization ≈ 24%: do not use absolute GPU thresholds.
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

Default values (`DefaultConfig`):

| Field | Value | Behavior |
|---|---|---|
| `GPUUtilThresholdPct` | 45 | General utilization that directly triggers a pause. |
| `GPUMemThresholdMiB` | 1500 | In-use memory that triggers a pause together with `GPUMemRequiresUtilPct`. |
| `GPUMemRequiresUtilPct` | 25 | Minimum utilization that must accompany high memory use. |
| `SustainedTicksRequired` | 3 | Consecutive high-load ticks before declaring a pause. |

The `NewEvaluator` constructor clamps invalid or overly aggressive values to
defaults; for example, `GPUUtilThresholdPct <= 25` is forced to 45% so that the
~24% idle level is not mistaken for a game. The clamps are: `GPUUtilThresholdPct
<= 25` → 45, `GPUMemThresholdMiB <= 0` → 1500, `SustainedTicksRequired <= 0` →
3, `GPUMemRequiresUtilPct <= 0` → 25, and `MinBatteryPct < 0` → 50.

`AllowBatteryFolding=false` in `Config{}` is intentional and conservative: a
zero-value `Config{}` never folds on battery. Callers that want `true` must start
from `DefaultConfig()`.

## Known limitations / open items

- `Evaluator` is not thread-safe.
- The thresholds (45% / 25% / 1500 MiB) have not yet been calibrated under real load.
- The battery-less desktop fixture is synthetic (there is no real Mac Studio dump).
- The `OnAC` semantics (`ExternalConnected || IPDInputPower>0`) still need to be
  confirmed with stale telemetry.

## Fixture privacy

Every serial (`AdapterDetails.SerialString`, `Serial`) is replaced with
`REDACTED_SERIAL`. Verify with `plutil -lint internal/detector/testdata/*.plist`
and do not commit unsanitized dumps.
