# Hardening Review: fix/m1-review-hardening

Date: 2026-11-09

This branch resolves the blockers A1, A2, M1, M2, M5 and M7 listed in
`docs/PENDING.md`. After those fixes were applied, an adversarial review
(red-team pass) was run against the changed code, including fuzzing.

## Fixes applied

| Item | Change |
| --- | --- |
| A1/M1 | `decodeXMLPlist` and `validateXMLPlist` in `internal/detector/darwin_ioreg.go` accept only well-formed XML with a `<plist>` root. Binary plist (`bplist`), OpenStep/text and invalid XML are rejected before `howett.net/plist` runs. The decode is wrapped in `recover`. |
| A2 | Iterative nesting-depth check with `encoding/xml`, limited by `maxPlistDepth = 64`. |
| M2 | `RestoreDefaultPowerLimit` (`internal/gpu/nvidia_linux.go`) clamps the default to the device constraints with `clampMW` (`internal/gpu/provider.go`). It returns an error if min > max or max == 0. |
| M5 | `NewEvaluator` resets `GPUUtilThresholdPct > 100` to 45 and `SustainedTicksRequired > 60` (`maxSustainedTicksRequired`) to 3. |
| M7 | `make build-linux` forces `CGO_ENABLED=0` and warns that it compiles the stub without TDP control. |

## Adversarial test findings

**Finding fixed during the review:** `SustainedTicksRequired` had no upper
bound. With 1,000,000 or `math.MaxInt`, the sustained-load pause never
triggered, so the device kept folding at full TDP (fail-open). This is
reproduced by `TestSustainedTicksHugeIsClamped` in
`internal/detector/detector_hardening_test.go` and fixed by the 60-tick cap.

**Also fixed:** if NVML reported a maximum constraint of 0 with `SUCCESS`,
the restore path would apply 0 W. It now returns an error instead.

**What held up:**

| Area | Result |
| --- | --- |
| XML validator | Fuzzing found no accepted input that falls through to howett's text parser, and no panic escapes. |
| Depth limit | Exact: 64 is accepted, 65 is rejected, including with namespace prefixes and self-closing elements. |
| XML bombs | Billion laughs, DOCTYPE, undefined entities, UTF-16, invalid UTF-8, NUL bytes and BOM are all rejected without panic in under 2 s. |
| Linear validation | 1, 4, 8 and 15 MiB inputs take 14, 33, 38 and 63 ms. |
| Base64 `<data>` | A 4 MiB base64 payload does not blow up memory. |
| Concurrency | 8 parsers × 10 runs pass with `-race`. |

**Fuzzing:**

- `FuzzValidateThenDecodeStaysXML`: 30 s, about 528k execs.
- `FuzzBuildSnapshotFromPlists`: 30 s, about 4.06M execs, plus an earlier
  20 s run of about 2.58M execs.
- No failures.

**Coverage (macOS, `-race`):** `internal/detector` 82.8 %, `internal/gpu` 100 %.

**New test files:**

- `internal/detector/darwin_ioreg_hardening_test.go` (darwin)
- `internal/detector/detector_hardening_test.go`
- `internal/gpu/provider_hardening_test.go`

**Residual risks:** see the "Residual risks" section (R1–R4) in
`docs/PENDING.md`. They are not repeated here.

## Verification on the Linux workstation (CachyOS)

`internal/gpu/nvidia_linux.go` (build tag `linux && cgo`) cannot be compiled
on macOS, so the M2 change was first reviewed by reading the code only. It was
then compiled and tested on the Linux workstation.

Result (2026-11-11, on `develop` after PR #7; `fix/m1-review-hardening` was
deleted on origin after the merge):

| Item | Value |
| --- | --- |
| OS | CachyOS (Arch Linux) |
| GPU | NVIDIA RTX 5060 |
| Driver | 620.70.06 |
| Power limits (NVML) | Default 145 W, Min 123 W, Max 145 W |
| `go env CGO_ENABLED` | `1` |
| `go list -f '{{.GoFiles}}' ./internal/gpu` | lists `nvidia_linux.go`, not `stub_linux.go` |
| `go build ./...`, `go vet ./...` | exit 0 |
| `make test` (uncached) | exit 0, 80 PASS, 0 FAIL |

Checklist:

- [x] **1. Check out `develop`** (`git fetch origin && git checkout develop && git pull`).
- [x] **2. Verify the environment** (`go version`, `go env CGO_ENABLED`,
  `ls -l /usr/lib/libnvidia-ml.so.1`, and a single manual
  `nvidia-smi -q -d POWER` to note Default/Min/Max Power Limit; the project
  forbids `nvidia-smi` in loops for telemetry).
- [x] **3. Build the real provider** (`go build ./...`, `go vet ./...`,
  `go list -f '{{.GoFiles}}' ./internal/gpu`).
- [x] **4. Run the tests** (`make test`). The macOS-only parser and fuzz tests
  (`darwin_ioreg*_test.go`) do not run on Linux.
- [x] **5. Record the result** (table above).
- [ ] **6. End-to-end NVML test.** `RestoreDefaultPowerLimit` has not been
  exercised against real hardware yet. The opt-in test
  `internal/gpu/nvidia_integration_linux_test.go`
  (`//go:build linux && cgo && nvml_integration`) lowers the limit to the
  device minimum, restores the default, and verifies both through
  `GetTelemetry`. A cleanup always restores the default TDP. Run it as root:

  ```bash
  sudo go test -tags nvml_integration -run TestNvidiaPowerLimitRoundTrip -v ./internal/gpu
  nvidia-smi -q -d POWER    # once, manually: Current Power Limit must be 145 W again
  ```
