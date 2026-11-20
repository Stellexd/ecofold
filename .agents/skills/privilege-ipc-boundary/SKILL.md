---
name: privilege-ipc-boundary
description: MUST READ when working on cmd/ecofold-daemon, IPC sockets, systemd services, or communication between the background daemon and the desktop GUI.
---

# Privilege Separation & IPC Boundary

1. **Zero Root in User Space:**
   - `cmd/ecofold-daemon` runs with administrative privileges (`CAP_SYS_ADMIN` / `root`) strictly to adjust hardware TDP limits and manage local sockets.
   - `cmd/ecofold-gui` MUST run as an unprivileged regular user.

2. **Unix Domain Socket Protocol:**
   - Daemon listens on `/run/ecofold/ecofold.sock` (or `/tmp/ecofold.sock` in development mode on macOS/Linux).
   - Validate all incoming socket payloads: never apply a requested Watt limit outside the hardware boundaries returned by `nvmlDeviceGetPowerManagementLimitConstraints`.

3. **Hardware Fail-Safe (Graceful Shutdown):**
   - Trap `os.Interrupt`, `syscall.SIGTERM`, and `syscall.SIGINT` in `cmd/ecofold-daemon`.
   - Before exiting, the daemon MUST restore the GPU Power Limit to its default 100% TDP (e.g., 145W) so the user's GPU is never left throttled if the service stops.