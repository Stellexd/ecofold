# 🌱 EcoFold

Smart GPU power limiter and game-aware scheduler for **Folding@home**. Donate compute without losing FPS or roasting your GPU.

## Architecture
- **`cmd/ecofold-daemon`**: Privileged background service managing GPU TDP via NVML and container/FAH state.
- **`cmd/ecofold-gui`**: Lightweight cross-platform system tray interface.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/GPU_PROVIDER.md](docs/GPU_PROVIDER.md), [docs/MACOS_TELEMETRY.md](docs/MACOS_TELEMETRY.md), and [docs/ROADMAP.md](docs/ROADMAP.md) for details.

## Power-state detection

On macOS, AC presence is derived from `AppleSmartBattery.ExternalConnected` when the key is present. Only when that key is absent does EcoFold fall back to `AppleSmartBattery.PowerDistribution.IPDInputPower > 0`. `IsCharging` is never used as the AC signal because it becomes `false` at full charge or during optimized-charge hold.

## Known limitations

- The battery-less desktop fixture used for testing is synthetic; a real Mac Studio dump is not yet available.
