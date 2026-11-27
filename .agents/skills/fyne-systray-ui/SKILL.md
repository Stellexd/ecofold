---
name: fyne-systray-ui
description: MUST READ when working on cmd/ecofold-gui, Fyne frontend/backend bindings, System Tray states, or UI components.
---

# Fyne GUI & System Tray Guidelines

1. **Minimal Resource Footprint:**
   - The GUI must consume 0% GPU when hidden in the System Tray and keep memory usage minimal.

2. **System Tray Visual States:**
   - `StateFolding` (Green): Folding active, GPU capped at eco Watt limit.
   - `StateGaming` (Purple/Blue): Game detected, FAH paused, GPU at 100% TDP.
   - `StateCooldown` (Amber): Game closed, waiting on hysteresis timer before resuming folding.
   - `StatePaused` (Gray): Manually paused by user.

3. **Human Impact & OS Adaptation:**
   - Display active Folding@home project metadata (e.g., Project ID `#186264` and disease research area fetched from `api.foldingathome.org/project/{id}`).
   - If the daemon reports `SupportsPowerLimit: false` (macOS Apple Silicon), automatically hide TDP Watt sliders and display Smart Pause controls only.