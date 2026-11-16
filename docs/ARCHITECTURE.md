# EcoFold Architecture

## 1. Privilege Separation (Daemon vs. GUI)
Changing GPU power limits via NVML (`nvmlDeviceSetPowerManagementLimit`) requires administrative privileges (`CAP_SYS_ADMIN` / root on Linux), whereas desktop UI and System Tray applications must run in user space.

- **`ecofold-daemon`**: Privileged background service managing hardware states (NVML/sysfs), game process detection, and Folding@home container/API control.
- **`ecofold-gui`**: Unprivileged desktop client communicating with the daemon over a local Unix Domain Socket: `/run/ecofold/ecofold.sock` on Linux (directory `0750` and socket `0660`, group `ecofold`) and `~/Library/Caches/ecofold/ecofold.sock` on macOS (`0700` / `0600`). The daemon checks the peer's credentials on every connection and never uses `/tmp`. Protocol and security model: [`internal/ipc/README.md`](../internal/ipc/README.md).

## 2. Cross-Platform Strategy
- **Linux (CachyOS / Arch / Distros)**: Full NVML power limiting + VRAM process inspection + Docker/FAH API control.
- **macOS (Apple Silicon)**: Uses `MockGPUProvider` for TDP controls (managed by Apple's SoC firmware) and focuses on Smart Pause when high-load processes are active.
- macOS measurement sources, electrical-state semantics, and platform limits are documented in [MACOS_TELEMETRY.md](MACOS_TELEMETRY.md).
