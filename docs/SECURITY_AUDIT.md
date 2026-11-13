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
