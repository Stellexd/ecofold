# Project Roadmap

- [ ] **Milestone 1 (v0.0.1):** GPU Provider Interface (NVML + Mock) & FAH Controller.
- [ ] **Milestone 2 (v0.0.2):** Hybrid Game Detector (VRAM threshold + Proton/Wine watcher) & Cooldown hysteresis.
- [ ] **Milestone 3 (v0.0.3):** Unix Socket IPC & Wails System Tray GUI.
- [ ] **Milestone 4 (v0.1.0):** macOS Apple Silicon (.dmg) & Arch Linux (AUR) packaging + Public Release.

## Immediate Plan (in order of impact)

The goal is to start wiring the pieces together so the project stops being a
library with empty binaries.

| Step | Package / File | What to do |
| --- | --- | --- |
| **1 (5 min)** | `internal/detector` + `internal/gpu` | Close the four quick residual risks (R1, R2, M6 and the `MinBatteryPct > 100` clamp) and remove the obsolete line from `internal/detector/README.md`. |
| **2 (M1 close)** ✅ | `internal/fah` | Create the `Controller` interface (`Pause(ctx)`, `Resume(ctx)`, `Status(ctx)`) with an implementation for FAH v8 / Docker and a `MockController` for tests. A local mock server that simulates port 7396 makes it possible to load-test it with Grafana k6. |
| **3 (M2 start)** ✅ | `internal/detector` (Linux + cooldown) | Replace `linux_stub.go` with the real collector that turns `gpu.Telemetry` and `gpu.GetGraphicsProcesses()` into a `SystemSnapshot`, and add `CooldownTicksRequired` to the `Evaluator` so folding does not resume the first second GPU load drops. Done with per-process NVML utilization; the Proton/Wine watcher (layer 2) is still pending. |
| **4 (the connector)** | `cmd/ecofold-daemon/main.go` | Write the main loop with `signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)`, guaranteeing that `gpuProvider.RestoreDefaultPowerLimit()` runs before the process exits. |
