# Development & Hardware Environments (`SYSTEM_SETUP`)

EcoFold is developed and validated across two primary physical environments to ensure strict cross-platform compatibility between Linux (`linux/amd64`) and macOS Apple Silicon (`darwin/arm64`).

## 1. Primary Hardware Testbed (Linux Workstation)
- **OS / Distro:** CachyOS (Arch Linux base), Kernel optimized, Wayland (KDE Plasma `kwin_wayland` + `Xwayland`)
- **GPU:** NVIDIA GeForce RTX 5060 (PCIe Bus `00000000:01:00.0`, Device Index `0`, 8151 MiB VRAM)
- **Driver / CUDA:** NVIDIA Driver `615.78.08` / CUDA `13.4` (`libnvidia-ml.so.1`)
- **NVML Power Constraints:**
  - **Minimum Power Limit (Eco/Folding):** `123 W` (`123000 mW`)
  - **Default / Maximum Power Limit (Gaming):** `145 W` (`145000 mW`)
- **Folding@home Runtime:** Docker container (`linuxserver/foldingathome:latest`, FAH Client `v8.5.6` / `FahCore_27`, exposed on TCP `127.0.0.1:7396`, ~860 MiB Compute VRAM usage)

## 2. Secondary Development & UI Environment (macOS Apple Silicon)
- **Device:** MacBook Air (Apple M4 SoC, `darwin/arm64`)
- **GPU Architecture:** Apple M4 Integrated GPU (Unified Memory Architecture)
- **Power Management Behavior:** Hardware TDP is managed autonomously by Apple's SoC firmware; NVML and manual watt capping are unsupported (`SupportsPowerLimit: false`).
- **EcoFold Runtime Role:**
  - Compiles without `cgo` / NVML dependencies using `//go:build darwin` (`MockProvider`).
  - Simulates an RTX 5060 profile in memory (`123 W` min / `145 W` max) for unit testing, IPC socket development, and Wails System Tray UI design.
  - Executes native Smart Pause logic (pausing Folding@home when high-load processes or games are active on macOS).

### GPU Power Limits (NVML Baseline)
Derived from `nvidia-smi -q -d POWER`:
- **Default / Maximum Power Limit:** 145.00 W (`145000` mW)
- **Minimum Power Limit (Eco Folding Baseline):** 123.00 W (`123000` mW)
- **Current Operational Power Draw (Folding):** ~115W – 122W

## 2. Driver & Runtime Environment
- **NVIDIA Driver Version:** 615.78.08
- **CUDA Version:** 13.4 (KMD / UMD 615.78.08)
- **Go Version:** 1.27.3

## 3. Folding@home Deployment
- **Execution Mode:** Docker Container (`linuxserver/foldingathome:latest`)
- **Container Name:** `foldingathome`
- **FAH Version:** v8.5.6 (`/app/fah-client`)
- **Active Core:** `FahCore_27` (`8.2.1`) processing OpenMM workloads (e.g., VRAM usage ~860 MiB during active compute).
- **Control Interface Port:** TCP/HTTP WebSocket on port `7396` (`0.0.0.0:7396->7396/tcp`).
- **Restart Policy:** `unless-stopped`

## 4. Cross-Platform Development Strategy
- **Linux Workstation (CachyOS):** Primary testbed for privileged hardware operations (`internal/gpu` via `go-nvml`), local Unix Domain Sockets IPC, and game detection (`internal/detector` watching Proton/Wine processes under Vulkan/DirectX).
- **macOS (Apple Silicon):** Secondary development and UI environment utilizing `MockGPUProvider` to simulate hardware telemetry and validate the Wails application interface (`cmd/ecofold-gui`) without requiring native NVIDIA drivers.