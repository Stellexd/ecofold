# GPU Provider (Milestone 1, Part 1)

The `internal/gpu` package defines a platform-neutral contract for GPU telemetry,
power-limit control, and graphics-process inspection. Linux uses NVIDIA's NVML
bindings; macOS builds use an in-memory mock for development on Apple Silicon.

## Public data types and contract

`Telemetry` is a snapshot containing the GPU name and UUID, temperature in
Celsius, current power use and power limit in watts, minimum/maximum/default
power limits in watts, used and total VRAM in MiB, and a
`SupportsPowerLimit` capability flag. `ProcessInfo` contains a process ID
(`PID`, `uint32`) and its GPU-memory use (`UsedVRAMMiB`, `uint64`).

The `Provider` interface has six methods:

| Method | Purpose |
| --- | --- |
| `Init() error` | Initialize the platform provider and its resources. |
| `Shutdown() error` | Release provider resources. |
| `GetTelemetry() (Telemetry, error)` | Read a telemetry snapshot. |
| `SetPowerLimit(watts int) error` | Set a power limit specified in watts. |
| `RestoreDefaultPowerLimit() error` | Restore the device's default power limit. |
| `GetGraphicsProcesses() ([]ProcessInfo, error)` | List processes using the GPU for graphics. |

All public power values use integer watts (W), and VRAM values use mebibytes
(MiB). The package's conversion helpers use integer division: NVML milliwatts
are converted to watts by dividing by 1,000, and bytes are converted to MiB by
dividing by 1,024 twice. Any fractional remainder is discarded. The reverse
conversion, watts to NVML milliwatts, multiplies by 1,000 and rejects negative
values or values greater than `math.MaxUint32 / 1000` before converting to
`uint32`, preventing overflow.

`validatePowerLimit` checks that a wattage is within the inclusive `[lo, hi]`
range. The Linux setter separately validates the converted milliwatt value
against the device's actual NVML constraints.

## Linux: NVML-backed NVIDIA provider

`nvidia_linux.go` has the `//go:build linux` constraint and is the only provider
implementation that imports `github.com/NVIDIA/go-nvml/pkg/nvml`. `NvidiaProvider`
opens NVIDIA GPU index `0`. `Init` initializes NVML and gets that device handle;
if handle lookup fails after NVML initialization, it shuts NVML down before
returning the error. Telemetry, setting/restoring a limit, and process queries
return an error if the device handle has not been initialized.

`GetTelemetry` reads the GPU name, UUID, temperature, power use, current limit,
supported limit constraints, default limit, and memory information. It reports
power in W and VRAM in MiB, and sets `SupportsPowerLimit` to `true`. NVML errors
are returned with the failing operation identified.

`SetPowerLimit` fetches `GetPowerManagementLimitConstraints` in milliwatts,
converts the requested watts safely to milliwatts, and rejects values outside
the inclusive device range before calling NVML to apply the limit. The default
limit is already supplied by NVML in milliwatts, so
`RestoreDefaultPowerLimit` applies that value directly. `GetGraphicsProcesses`
uses NVML's graphics-running-process list and converts each process's GPU-memory
usage from bytes to MiB.

## macOS: Apple Silicon development mock

`mock_darwin.go` has the `//go:build darwin` constraint and uses no NVML or
external C library. `MockProvider` stores simulated state in memory and guards
it with a read/write mutex. On `Init`, it provides a simulated RTX 5060 profile:
minimum and maximum power limits of 123 W and 145 W, respectively, with a
145 W default/current limit; it reports `SupportsPowerLimit=false`. Its other
initial telemetry values are 42 °C, 75 W power use, and 512 MiB used out of
8,192 MiB total VRAM. The simulated graphics-process list starts empty.

Because Apple Silicon does not support this provider's power-limit controls,
`SetPowerLimit` and `RestoreDefaultPowerLimit` return an unsupported-operation
error rather than changing state. `Shutdown` is a no-op. `NewProvider` returns
the platform-specific implementation (`NvidiaProvider` on Linux and
`MockProvider` on macOS).

## Tests and cross-platform maintenance

`provider_test.go` tests the platform-independent range and unit-conversion
helpers, including boundary values, integer truncation, and overflow/negative
input handling. These tests do not initialize NVML and do not require root
privileges or a physical GPU.

`mock_darwin_test.go` is guarded by `//go:build darwin`. It checks that the mock
satisfies `Provider`, reports the expected capability and power-limit values,
rejects power-limit changes without mutating state, and supports its remaining
contract methods.

Keep platform implementations isolated with their build constraints: NVML
imports belong only in the Linux implementation, while the Darwin mock must
remain usable without NVML or C dependencies. When changing the `Provider`
contract, preserve matching method behavior and compatible telemetry/process
units across both implementations so the package continues to build on Linux
and macOS.
