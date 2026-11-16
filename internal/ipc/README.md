# `internal/ipc`: daemon control socket

The channel between `ecofold-daemon` (root on Linux) and its unprivileged
clients, today the GUI. One JSON message per line over a Unix domain socket.

The package imports only the standard library and `golang.org/x/sys`. It does
not import `internal/daemon`: the daemon plugs in through `ipc.Handler`, and
the adapter lives in `cmd/ecofold-daemon/ipc.go`.

Status: implemented and tested on macOS and, unprivileged, on CachyOS. The
root, group and systemd checks are still pending on CachyOS; see
`docs/PENDING.md`.

## Socket

| | Linux | macOS |
| --- | --- | --- |
| Default path (`-socket-path`) | `/run/ecofold/ecofold.sock` | `~/Library/Caches/ecofold/ecofold.sock` |
| Directory | `0750`, owner root, group `ecofold` | `0700`, owner the user |
| Socket | `0660`, owner root, group `ecofold` | `0600` |
| Group (`-socket-group`) | `ecofold` | none |
| Who may connect | root, the daemon's user, members of the group | the daemon's user |

There is no fallback to `/tmp` or any shared directory. `-socket-path ""`
turns IPC off.

How the daemon opens the socket (`ipc.Listen`):

1. The path must be absolute and shorter than `sun_path` (108 bytes on Linux,
   104 on macOS).
2. The directory is created if missing. If it exists it must be a real
   directory, not a symlink, owned by the daemon's user; otherwise the socket
   is not opened.
3. Whatever is at the socket path is checked with `lstat`. A socket nobody
   listens on is removed. A socket that accepts a connection means another
   daemon is running (`ErrAlreadyRunning`). A symlink, a regular file or a
   directory is an error and is never removed.
4. The socket is bound under a temporary umask of `0177`, so it is `0600`
   from the first instant. It is changed to `0660` only after the group owns
   it. Ownership and mode are final before the first `accept`.
5. If the group does not exist, or the socket cannot be given to it, the
   socket stays `0600`: only root can connect, and a warning says so. The
   daemon still starts.

If the socket cannot be opened at all, the daemon logs the error and runs
without IPC. Capping and restoring the GPU power limit does not depend on the
GUI's channel.

### Group ownership under systemd

The unit keeps `CapabilityBoundingSet=CAP_SYS_ADMIN`, so the daemon has no
`CAP_CHOWN` and cannot give a file to a group root is not a member of. The
unit therefore runs two privileged commands before the daemon starts:

```
ExecStartPre=+-/usr/bin/chgrp ecofold /run/ecofold
ExecStartPre=+-/usr/bin/chmod 2750 /run/ecofold
```

The setgid bit makes the socket inherit the group when the daemon binds it.
The daemon checks the result: if the directory or the socket does not belong
to the group, it tries `chown` itself (this works with full root, for example
`sudo make run-daemon`) and otherwise falls back to `0600`.

Create the group and add the desktop user once:

```
sudo groupadd --system ecofold
sudo gpasswd -a "$USER" ecofold    # log in again afterwards
```

## Peer credentials

Every connection is checked on `accept`, before a single byte is read, with
credentials the kernel recorded for the peer:

- **Linux:** `SO_PEERCRED`. Allowed: uid 0, the daemon's own uid, or a member
  of the socket group. Membership is the peer's primary gid, or the group
  listed for its uid in the user database (`os/user`, which reads
  `/etc/passwd` and `/etc/group` without cgo), cached for 30 s per uid.
- **macOS:** `LOCAL_PEERCRED`. Allowed: the daemon's own uid only.

A refused peer is disconnected without a response. The refusal is logged as a
warning with uid, gid and pid, at most once every 5 s.

Supplementary membership comes from the user database, not from the peer
process. A user added to the group is accepted without logging in again, as
far as this check goes; a user removed from it is refused within 30 s even if
an old session still holds the group.

## Protocol

Each message is one line of JSON ending in `\n`, at most 64 KiB
(`ipc.MaxMessage`) without the newline. A connection carries any number of
requests, answered one at a time and in order.

Request:

```json
{"v":1,"id":"42","cmd":"set_eco_watts","args":{"watts":120}}
```

| Field | Meaning |
| --- | --- |
| `v` | Protocol version. Must be `1`. |
| `id` | Chosen by the client, at most 64 bytes, echoed in the response. |
| `cmd` | One of the commands below. |
| `args` | Object, only for the commands that take arguments. |

Response:

```json
{"v":1,"id":"42","ok":true,"data":{ ...status... }}
{"v":1,"id":"42","ok":false,"error":{"code":"out_of_range","msg":"..."}}
```

Unknown fields are rejected everywhere, in the request and in `args`.

### Commands

Every successful command returns the daemon status as `data`.

| Command | Args | Effect |
| --- | --- | --- |
| `status` | none | Returns the last published status. Never waits for the control loop. |
| `force_pause` | none | Manual pause: restores the default power limit, pauses Folding@home and keeps it paused until `force_resume`, whatever the detector decides. If the user had already paused FAH, that pause stays the user's. |
| `force_resume` | none | Clears the manual pause and resumes folding now, eco limit first. |
| `set_eco_watts` | `{"watts": N}` | Sets the power limit used while folding. `N` must be within `min_power_limit_w`..`max_power_limit_w`; it is never clamped. Applied immediately if the eco limit is in effect. |
| `set_pause_processes` | `{"patterns": ["blender","obs*"]}` | Replaces the pause list. Same syntax as `-pause-processes`, one pattern per element, at most 64, no commas. `[]` clears the list. |

Notes:

- `force_resume` also resumes a pause the user made in FAH itself. Asking for
  it is the user's consent; it is the one exception to "the daemon never
  resumes a user pause" (CH-10, invariant I1).
- `force_resume` does not resume while the detector's current decision is a
  pause (`game_active`, `on_battery`), or if the eco limit cannot be applied
  (`cap_failed`). The manual pause is cleared in all three cases, so the
  daemon resumes its own pause automatically once conditions allow.
- In Degraded mode, or without permission to set power limits, `force_resume`
  resumes at the default limit. The status shows `degraded` or
  `power_enabled: false`.
- Nothing is persisted. After a daemon restart the eco limit and the pause
  list come from the command-line flags again, and the manual pause is gone.
  FAH stays paused; if the daemon made the pause, the pause marker makes it
  resume after the startup hold.
- On macOS `set_pause_processes` answers `unsupported`, like `set_eco_watts`:
  the macOS collector cannot see processes, so a pause list would do nothing.

### Error codes

| Code | Meaning |
| --- | --- |
| `bad_request` | Malformed JSON, wrong version, unknown field, bad `args`, invalid pause pattern. |
| `unknown_cmd` | `cmd` is not one of the five commands. |
| `too_large` | The request is over 64 KiB. The connection is closed. |
| `unsupported` | The device or platform cannot do it (no power limit control, no pause list). |
| `out_of_range` | Watts outside the device constraints. |
| `cap_failed` | The driver rejected the power limit. The previous value stays. |
| `game_active` | Not resumed: the detector sees a game or GPU load. |
| `on_battery` | Not resumed: battery guard. |
| `fah_unavailable` | Folding@home did not confirm the pause or resume. |
| `busy` | The control loop did not take the command within 3 s. A command the loop never took has no effect. |
| `stopping` | The daemon is shutting down. |
| `internal` | Unexpected failure. |

After `fah_unavailable` the daemon keeps retrying every tick, except for a
`force_resume` of a pause the user made: that one is not retried, send it
again.

### Status

```json
{
  "mode": "folding",
  "manual_override": "none",
  "paused_by_us": false,
  "degraded": false,
  "power_capable": true,
  "power_enabled": true,
  "cap_applied": true,
  "power_limit_w": 123,
  "default_power_limit_w": 145,
  "min_power_limit_w": 100,
  "max_power_limit_w": 145,
  "eco_watts": 123,
  "gpu": {"name": "...", "temp_c": 61, "power_usage_w": 118, "used_vram_mib": 812, "total_vram_mib": 8192},
  "battery": {"present": false, "on_ac": true, "charging": false, "pct": 100},
  "fah": {"paused": false, "reachable": true, "observed_at": "2026-11-16T02:13:01-03:00"},
  "game_sessions": [],
  "pause_processes": ["blender"],
  "pause_matches": [],
  "updated_at": "2026-11-16T02:13:01-03:00"
}
```

- `mode`: `starting`, `folding`, `folding_eco`, `paused_load`,
  `paused_battery`, `paused_manual` or `degraded`.
- `manual_override`: `none` or `pause`.
- `paused_by_us`: the current FAH pause is the daemon's, so the daemon will
  resume it.
- The status is a snapshot taken at `updated_at`, at the end of the last tick
  or command. Telemetry and game sessions are the last known values.
- `fah` is what the daemon learned the last time it talked to FAH
  (`observed_at`; the zero time means never). The daemon only talks to FAH
  when it pauses or resumes, so a pause made in FAH's own UI does not show
  here until then.

## Limits

| Limit | Value |
| --- | --- |
| Message size | 64 KiB; over it: `too_large`, then close |
| Read deadline | 2 s for a whole request, and for the idle wait between requests |
| Write deadline | 2 s per response |
| Concurrent connections | 32; one more is closed on accept |
| Command wait | 3 s for the control loop, then `busy` |

The server closes idle connections after 2 s. `ipc.Client` redials on the
next call. A panic while serving a connection closes that connection and is
logged; the daemon keeps running.

## Shutdown

On SIGTERM or SIGINT the socket closes first: no new connections, open ones
are closed, requests in flight get up to 1 s. Then the control loop stops and
the fail-safe restores the default power limit. The socket file is removed.
After a SIGKILL the file stays behind and the next start replaces it.

## Client

```go
c, err := ipc.Dial(ctx, path)
if err != nil {
    // daemon not running
}
defer c.Close()

raw, err := c.Call(ctx, ipc.CmdSetEcoWatts, map[string]int{"watts": 120})
var re *ipc.RemoteError
if errors.As(err, &re) {
    // re.Code is one of the codes above
}
```

`Call` honors the context's deadline and cancellation (5 s if it has none).
After a transport error the outcome of a state-changing command is unknown:
read the status before retrying.

By hand:

```
printf '{"v":1,"id":"1","cmd":"status"}\n' | nc -U /run/ecofold/ecofold.sock
```

## Tests

```
go test -race ./internal/ipc/ ./cmd/ecofold-daemon/
go test -run '^$' -fuzz FuzzReadMessage -fuzztime 30s ./internal/ipc/
```

Tests use `os.MkdirTemp("", "ef")` instead of `t.TempDir()`: the test name in
the path would overflow `sun_path` on macOS.
