# 🌱 EcoFold

Smart GPU power limiter and game-aware scheduler for **Folding@home**. Donate compute without losing FPS or roasting your GPU.

## Architecture
- **`cmd/ecofold-daemon`**: Privileged background service managing GPU TDP via NVML and container/FAH state.
- **`cmd/ecofold-gui`**: Lightweight cross-platform system tray interface.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/GPU_PROVIDER.md](docs/GPU_PROVIDER.md), and [docs/ROADMAP.md](docs/ROADMAP.md) for details.
