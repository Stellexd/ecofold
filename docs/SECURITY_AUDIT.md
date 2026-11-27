# Security Audit: EcoFold

Date: 2026-11-10
Method: `security-audit` skill (`.claude/skills/security-audit`), full-audit workflow, `quick` profile.

## 1. Run scope

| Item | Value |
| --- | --- |
| Source ref | `1b2a9e8` on `fix/m1-review-hardening`, clean worktree |
| Profile | `quick`: one hunter wave, one final coverage critic, one independent verifier per candidate. **This is a partial pass.** |
| Scope | Tracked files: `cmd/`, `internal/`, `go.mod`, `go.sum`, `Makefile`, `docs/` |
| Out of scope | `scripts/` (gitignored local developer tools, untracked at this commit) |
| Execution | Source-only. No target code was built, tested, or run. No network access was used. |
| Prior runs | None. This is the first skill-based run. `docs/HARDENING_REVIEW.md` is an earlier, non-skill robustness review. |
| Budget | Not set. 2 delegated agents were used (1 candidate verifier, 1 final critic). |
| Workflow deviation | Because the target is small (~2.8k lines of Go), reconnaissance and the single hunter wave were done by the coordinating agent instead of separate delegated agents. Candidate verification and the coverage critique were done by fresh, independent agents. |

Run artifacts (`run-metadata.json`, `architecture.md`, `coverage-ledger.json`, `findings.json`) are stored outside the repository, in `~/security-audit-skill/ecofold/run-1/`. Both skill validators pass: 10 coverage units and 1 finding record.

## 2. Security posture summary

There are **no confirmed vulnerabilities** at this commit. The project does not yet have a reachable attack surface:

- `cmd/ecofold-daemon` and `cmd/ecofold-gui` are empty `main()` functions.
- There is no IPC, no network listener, no configuration loader, and no Folding@home controller.
- The two implemented packages (`internal/gpu`, `internal/detector`) only consume data from trusted OS sources: NVML and the root-owned `/usr/sbin/ioreg`.

The main risk is in the **planned design**: the privilege boundary between the unprivileged GUI and the root daemon. That design should be fixed before Milestone 3 (Unix socket IPC) is implemented. See section 4.

## 3. Confirmed findings

None.

## 4. Needs validation

These leads are grounded in source or design documents but cannot be demonstrated yet. They have no severity and are not confirmed vulnerabilities.

### NV-1: Planned root-daemon IPC socket allows a `/tmp` path and specifies no peer authentication

Fingerprint: `ecofold:ipc:planned-tmp-socket-root-daemon`

**Status (2026-11-16): Fixed and verified on Linux.** Fixed in `5f165b9`
(socket and peer credentials), `de35037` (daemon integration) and `933170e`
(systemd unit); verified on CachyOS at `933170e`. The text below the tables
describes the design as audited on 2026-11-10.

| Recommendation | State |
| --- | --- |
| 1. No `/tmp` socket | Done. Linux: `/run/ecofold/ecofold.sock`. macOS: `os.UserCacheDir()`. There is no fallback path. |
| 2. Root-owned `0750` directory, socket `0660`, group `ecofold` | Done. A stale socket is replaced only after `lstat` shows a socket nobody listens on; a symlink or any other file is refused. Without the group the socket is `0600`. |
| 3. Peer credentials on every connection | Done: `SO_PEERCRED` (Linux), `LOCAL_PEERCRED` (macOS), checked on accept before reading. |
| 4. Watt validation in the daemon | Done: `set_eco_watts` rejects values outside the NVML constraints (`out_of_range`), and `SetPowerLimit` keeps its own range check. |
| 5. Align the documents | `docs/ARCHITECTURE.md` and `internal/ipc/README.md` done. The local `privilege-ipc-boundary` skill (gitignored) still mentions `/tmp/ecofold.sock` for development. |

Verification on CachyOS (2026-11-16, kernel 7.3.3, RTX 5060, real FAH client,
daemon running as root under the systemd unit):

| Check | Result |
| --- | --- |
| `go test -race ./...` with `CGO_ENABLED=1`, normal user | Pass |
| `internal/ipc` tests as root (race build) | 34 pass, 1 skipped (needs a supplementary group) |
| `/run/ecofold` and the socket after start, and after a stop and start | `root:ecofold 750` and `root:ecofold 660` |
| Member of `ecofold`: `status`, `force_pause`, `force_resume`, `set_pause_processes` | Pass. Under the manual pause the real limit was 145 W and FAH was paused; `force_resume` applied 123 W, then resumed FAH. |
| `set_eco_watts` | 130 W changed the real limit (`nvidia-smi`: 130 W) and 123 W restored it; 500, 122 and 120 W were refused with `out_of_range` (device range 123-145 W) and the limit did not move. |
| `nobody`, normal file modes | `connect`: permission denied |
| `nobody`, with the directory and socket opened to everyone for the test | Connection closed with no response; journal: `peer not authorized ... uid=65534`; the `force_pause` it sent had no effect |
| `systemctl stop` | Socket removed, limit 145 W |

Found during this verification: the first unit tried to hand the directory to
the group from `ExecStartPre`. It had no effect, the daemon could not `chown`
without `CAP_CHOWN`, and the socket fell back to `root:root 0600`: closed to
everyone, as designed, but unusable. `933170e` adds `CAP_CHOWN` to the unit's
bounding set and removes those lines. `SocketMode`/`SocketGroup` are not used
because they only apply to socket-activated units.

Not verified: a start under systemd with the `ecofold` group missing (covered
only by a unit test), and the full security review of the socket, both in
`docs/PENDING.md`.

**Trace**

| Kind | Location | Description |
| --- | --- | --- |
| entrypoint | `docs/ARCHITECTURE.md:7` | The GUI talks to the daemon over a Unix socket at "`/run/ecofold.sock` or `/tmp/ecofold.sock`". The `/tmp` option has no dev-only qualifier. |
| propagation | `cmd/ecofold-daemon/main.go:3` | `main()` is empty. No listener or protocol exists yet; IPC is Milestone 3 (`docs/ROADMAP.md:5`). |
| sink | `internal/gpu/nvidia_linux.go:128` | `SetPowerLimit` calls `SetPowerManagementLimit` after only an NVML min/max range check (lines 117–127). It has no notion of who the caller is. |

**Boundary at risk:** an unprivileged local user versus the root daemon's control of the GPU power limit.

**Hypothesis:** if the root daemon ever listens in world-writable `/tmp`, or accepts connections without checking peer credentials:

- any local user can connect and lower or restore the TDP, or interfere with pause/resume (tampering or denial of the control function);
- any local user can pre-create the socket path and impersonate the daemon to the GUI.

Hardware damage is unlikely, because requested values are limited to the NVML constraint range.

**Additional facts from verification**

- `.agents/skills/privilege-ipc-boundary/SKILL.md` prescribes `/run/ecofold/ecofold.sock`, and limits `/tmp/ecofold.sock` to development mode. `docs/ARCHITECTURE.md:7` uses a different production path and does not mark `/tmp` as dev-only.
- No tracked file specifies socket mode, owner, group, a systemd unit, or `SO_PEERCRED` checks. The skill file itself is gitignored.

**Blockers**

- No daemon listener, IPC protocol, or non-test caller of `SetPowerLimit` exists at `1b2a9e8`.
- No permission or packaging specification exists to check.

**Bounded local next step (once the daemon exists):** add a Linux integration test that asserts all of the following:

- The listener starts as root, and a connection from an unprivileged dummy uid is rejected by an `SO_PEERCRED` uid/gid check.
- The socket is mode `0660` or stricter, inside a root-owned `0750` directory.
- When the socket path (or a symlink) was pre-created by another user, the daemon refuses to start, or unlinks the path only after an `lstat` ownership check.

**Owner-observed deployment check:** the packaged systemd unit must set `RuntimeDirectory=ecofold`, `RuntimeDirectoryMode=0750`, `SocketMode=0660`, and a dedicated `SocketGroup`. Release builds must not contain a `/tmp` fallback.

**Recommended design fix (smallest)**

1. Remove `/tmp/ecofold.sock` for the root daemon. For an unprivileged macOS mock in development, use `$XDG_RUNTIME_DIR` or `os.UserCacheDir()`, never `/tmp`.
2. Listen on `/run/ecofold/ecofold.sock` in a root-owned `0750` directory, with the socket at `0660` and group `ecofold`.
3. Check peer credentials on every connection: `SO_PEERCRED` on Linux, `getpeereid`/`LOCAL_PEERCRED` on macOS.
4. Keep strict validation of watt values on the daemon side, as it is today.
5. Align `docs/ARCHITECTURE.md:7` with the IPC skill, and add the mode, group, and peer-credential requirements to that skill.

## 5. Hardening notes (not findings)

None of these has a lower-trust principal that controls the input today. They are recorded so they are handled when the daemon arrives.

| Item | Location | Note |
| --- | --- | --- |
| H1 Unbounded `ioreg` output | `internal/detector/darwin_ioreg.go:301` | `Output()` buffers all of stdout before the 16 MiB check. The input is root-owned `ioreg`, so this is robustness only. Already tracked as R1 in `docs/PENDING.md`. |
| H2 uint64→int truncation | `internal/detector/darwin_ioreg.go:187` | A huge `Device Utilization %` would wrap to a negative value and never trigger a pause (fail-open). The source is kernel IOKit data, so there is no attacker. Clamp to 0–100 anyway. |
| H3 GPU process list as a future decision input | `internal/gpu/nvidia_linux.go:164` | `GetGraphicsProcesses` returns processes of every local user. When Milestone 2 game detection uses it, any local user can steer the root daemon's TDP decisions by starting a GPU process. Treat this as an accepted, documented product behavior, and keep the daemon's actions limited to the NVML range. |
| H4 `sudo` on a user-writable binary | `Makefile:27` | `make run-daemon` runs `./bin/ecofold-daemon` as root while the binary is owned by the developer. This is fine for development (same principal). Packaged installs must use a root-owned binary and a systemd unit, not this target. |
| H5 Folding@home control port | `docs/SYSTEM_SETUP.md:39` vs `:12` | The workstation notes show the FAH WebSocket control port published as `0.0.0.0:7396`, while line 12 says `127.0.0.1:7396`. Publish it on `127.0.0.1` only, because the FAH v8 control API should not be reachable from the LAN. This is environment configuration, not EcoFold code. |
| H6 Root daemon lifecycle | `internal/gpu/nvidia_linux.go` | `docs/PENDING.md` M3 and M4 (process-global `nvml.Shutdown`; mutex held during blocking NVML calls) affect fail-safe TDP restore on SIGTERM. Resolve them in the daemon milestone. |

## 6. Positive patterns observed

- `exec.CommandContext` uses an absolute path (`/usr/sbin/ioreg`), passes arguments as an argv slice with no shell, uses a 2 s timeout, and receives only constant class names (`darwin_ioreg.go:301`).
- The plist parser is guarded in depth: it accepts XML only, rejects `bplist`, limits size and nesting depth (64) before the recursive decoder runs, and recovers from panics (`darwin_ioreg.go:236-290`).
- Power-limit writes check for negatives and `uint32` overflow (`provider.go` `wToMW`) and check the NVML min/max range. The restore path clamps the value and rejects `max == 0`.
- NVML is confined to `//go:build linux && cgo`. The no-cgo stub and the darwin mock fail closed on `SetPowerLimit`.
- Dependencies are minimal (2 direct), pinned, and checksum-bound in `go.sum`, with no `replace` directives.
- There are no secrets, environment-variable reads, or file writes in tracked source.

## 7. Coverage summary

| Status | Units |
| --- | --- |
| covered | 6: ioreg exec (injection), plist parser (resource exhaustion), evaluator config (business logic), NVML `SetPowerLimit` (business logic), dependencies (supply chain), `Makefile run-daemon` (access control) |
| candidate | 1: daemon IPC → NV-1 |
| deferred | 2, added by the final critic and not reviewed in this quick run: stub/mock provider parity (`stub_linux.go`, `mock_darwin.go`); GPU process list as a decision input (`nvidia_linux.go:164`) |
| out_of_scope | 1: `scripts/` (untracked) |

**Final critic result:** stop = true. There were no reassignments. Two missing units were proposed and recorded as `deferred` under the quick-profile rule.

**Limits**

- No known-vulnerability scan of dependencies was run (`govulncheck` is not installed and network access was not used).
- `nvidia_linux.go` was reviewed by reading only.
- This quick run does not claim complete coverage. Rerun with the `standard` profile once Milestone 3 adds the daemon, IPC, and Wails GUI. Those components will be the real attack surface.

## 8. IPC socket review (2026-11-20)

Method: `security-audit` skill in guidance mode, a focused source review by
one agent. It is not the full `standard` workflow: there were no delegated
hunters, verifiers or coverage ledger, and no run artifacts. Existing tests
were the only code run.

| Item | Value |
| --- | --- |
| Source ref | `be42348` on `feat/m3-macos-process-detection`, clean worktree |
| Scope | `internal/ipc` (server, protocol, peer credentials, paths on Linux and macOS), `internal/daemon/command.go`, `cmd/ecofold-daemon/ipc.go`, `deploy/systemd/ecofold-daemon.service` |
| Out of scope | The GUI's other inputs (Folding@home project API), FAH WebSocket client, detectors |
| Threat model | A local user outside root, the daemon's user and the `ecofold` group (Linux); any other local user (macOS). Members of `ecofold` are trusted to pause, resume, set the eco limit and the pause list. |

**Result: no confirmed findings.** NV-1 (section 4) is resolved in code:

- The socket lives in `/run/ecofold` (Linux) or the user's cache directory
  (macOS), never in `/tmp`. The directory must be a real directory owned by
  the daemon's user; it is set to `0750` (`0700` on macOS).
- `bind(2)` runs under umask `0177`, so the socket is `0600` from creation;
  it becomes `0660` only after its group really is `ecofold`. A missing
  group leaves it `0600` (`resolveGroup`, fail-closed).
- Only a dead socket is removed at startup; a live one is
  `ErrAlreadyRunning`, and a symlink, file or directory is refused.
- Peers are authenticated with `SO_PEERCRED` (Linux) or `LOCAL_PEERCRED`
  (macOS) before any byte is read; a refused peer is closed without an
  answer. Supplementary groups come from the user database with a 30 s
  cache, so a user removed from `ecofold` loses access within 30 s.
- Each message is capped at 64 KiB while it is read, one 2 s deadline covers
  a whole message, connections are capped at 32, and a panic closes only its
  connection.
- Requests are strict JSON (unknown fields and trailing data rejected) with a
  closed command list. Watts are checked against the NVML range without
  clamping. Pause patterns are capped (64, bounded length), must be process
  names, allow `*` only at the end, cannot match everything and cannot match
  the Folding@home processes.
- `force_resume` is refused during a game, battery or thermal pause.

**Notes (not findings)**

- I1: membership in `ecofold` is read from the user database, not from the
  peer process's live groups. A same-uid process that dropped the group (a
  sandbox) would pass if it could connect; today the `0750` directory stops
  it before `connect(2)`.
- I2: `pause_matches` can name processes of other users to a member of
  `ecofold`. On Linux `/proc` already shows them unless `hidepid` is set.
- I3: the unit hardening (`SystemCallFilter`, `ProtectKernelModules`,
  `RestrictNamespaces`, `MemoryDenyWriteExecute`) is still open in
  `docs/PENDING.md`.

**Still to see on hardware:** the unit started without the `ecofold` group
must log `socket group not found` and leave the socket `0600`. A unit test
covers it; it has not been run on CachyOS.
