---
name: go-hardware-arch
description: MUST READ when creating or modifying files inside internal/gpu, internal/detector, or internal/fah. Enforces Go build tags for Linux (NVML/CachyOS) and macOS (Apple Silicon Mock) compatibility.
---

# Hardware Abstraction & Cross-Platform Rules
When writing or modifying hardware, GPU, or process detection code, enforce these constraints:

1. **Strict Build Tag Isolation:**
   - Linux NVML implementation (`internal/gpu/nvidia_linux.go`): Must start with `//go:build linux` and is the ONLY place allowed to import `github.com/NVIDIA/go-nvml/pkg/nvml`.
   - macOS Mock implementation (`internal/gpu/mock_darwin.go`): Must start with `//go:build darwin` and use zero external C/NVML libraries so `go build ./...` succeeds natively on Apple Silicon (`darwin/arm64`).

2. **Power Unit Conversion (Watts vs. Milliwatts):**
   - Public Go interfaces (`gpu.Provider`) must strictly accept and return integer **Watts (W)** (e.g., `145` for gaming, `123` for idle folding on RTX 5060).
   - Inside `nvidia_linux.go`, multiply Watts by `1000` before calling `nvmlDeviceSetPowerManagementLimit` (which expects **milliwatts**, e.g., `123000`).

3. **Hybrid Game Detection (`internal/detector`):**
   - Never execute `nvidia-smi` or shell subcommands in a polling loop.
   - Layer 1: Poll `nvmlDeviceGetGraphicsRunningProcesses` every 1.5–2s. Trigger `StatePausedLoad` if non-ignored processes exceed the VRAM threshold (`GPUMemThresholdMiB` = 1500 MiB, rule `>=`) and utilization meets or exceeds `GPUMemRequiresUtilPct` (25%).
   - Layer 2: Detect active Wine/Proton (`wineserver`, `reaper`, `.exe` payloads like `deadlock.exe`) and apply a configurable exit cooldown (2–3 minutes) before restoring folding state.
   - *Status:* The Linux NVML-based detection layers are pending implementation; the architecture supports them, but the current repository focuses on the macOS mock path.

4. **macOS Apple Silicon Telemetry & Detection (`//go:build darwin`):**
   - It is permitted to call `ioreg -a -r -d 1 -c AppleSmartBattery` and `ioreg -a -r -d 1 -c IOAccelerator` **only during initialization or one-shot refresh**, not in a tight polling loop.
   - The resulting XML property list must be parsed **in memory** using `howett.net/plist` (pure Go, no cgo). Never link against IOKit frameworks.
   - Decode `AppleSmartBattery` for electrical state and `IOAccelerator` for GPU utilization.
   - Electrical-state mapping must follow `MACOS_TELEMETRY.md` and `internal/detector/README.md`:
     - `OnAC = ExternalConnected`, cross-checked by `PowerDistribution.IPDInputPower > 0` when present.
     - `IsCharging` comes from the `IsCharging` key or from a positive signed `InstantAmperage`; it indicates *charging vs. plugged-in maintenance*, never AC presence.
     - Signed values that IOKit stores as unsigned (`InstantAmperage`, `Amperage`, `BatteryPower`, `SystemLoad`) must be decoded as `uint64` and reinterpreted with `int64(v)` (two's-complement).
     - If `BatteryData.BatteryPower` is absent, fall back to `round((Voltage_mV * InstantAmperage_mA) / 1000.0)`.
     - `TimeRemaining` values `65535` or `0` mean unknown and map to `nil`.
     - An empty `<array/>` from `AppleSmartBattery` means no battery (e.g., Mac mini): report `HasBattery=false`, `OnAC=true`, `BatteryPct=100`, without error.
   - Decode `IOAccelerator` generically: pick the first node that contains `PerformanceStatistics`, without hardcoding classes such as `AGXAcceleratorG16G`. Map `Device Utilization %`, `Renderer Utilization %`, `Tiler Utilization %` and convert `In use system memory` from bytes to MiB. Missing performance keys must degrade to `0`, never panic.
   - All macOS telemetry is read-only and unprivileged; GPU power limiting remains unsupported on Apple Silicon (`MockGPUProvider` returns `SupportsPowerLimit=false`).
