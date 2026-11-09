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

## Pending: verification on the Linux workstation (CachyOS)

`internal/gpu/nvidia_linux.go` (build tag `linux && cgo`) cannot be compiled
on macOS. The M2 change was therefore reviewed by reading the code only. It
must be compiled and tested on the Linux workstation before tagging.

Hardware:

- OS: CachyOS (Arch Linux)
- GPU: NVIDIA RTX 5060 (default 145 W, eco 123 W)
- Library: `libnvidia-ml.so.1`

Checklist:

- [ ] **1. Prerequisites.** The branch must be pushed from the Mac. On CachyOS:

  ```bash
  git fetch origin && git checkout fix/m1-review-hardening && git pull
  ```

- [ ] **2. Verify the environment.**

  ```bash
  go version
  go env CGO_ENABLED        # must print 1
  ls -l /usr/lib/libnvidia-ml.so.1
  nvidia-smi -q -d POWER    # run ONCE, manually, only to note Default/Min/Max Power Limit
  ```

  The project forbids `nvidia-smi` in loops for telemetry. This single call is
  a manual reference check, not part of the code path.

- [ ] **3. Build the real provider.**

  ```bash
  go build ./...
  go vet ./...
  go list -f '{{.GoFiles}}' ./internal/gpu   # should list nvidia_linux.go
  ```

  This confirms that `nvidia_linux.go` compiles and `stub_linux.go` does not
  replace it. `make build` is not enough yet: it only builds `cmd/*`, whose
  `main` packages do not import `internal/gpu` so far.

- [ ] **4. Run the tests.**

  ```bash
  make test   # equivalent to: go test -race -v ./...
  ```

  The macOS-only parser and fuzz tests (`darwin_ioreg*_test.go`) do not run on
  Linux. `detector_test.go`, `detector_hardening_test.go` and the tests in
  `internal/gpu` (including `TestClampMW`) do run.

- [ ] **5. Record the result.** Report the `make test` output, the Go version
  and the NVIDIA driver version. If anything fails, copy the exact error for
  the coder.

- [ ] **6. Known limitation.** There is no integration test against real NVML
  and no daemon yet, so `RestoreDefaultPowerLimit` is not exercised end to end.
  Recommended follow-up: add an opt-in integration test behind a build tag
  (for example `//go:build linux && cgo && nvml_integration`), run with
  `sudo go test -tags nvml_integration ./internal/gpu`. It should read the
  constraints, apply 123 W, restore the default, and verify the result with
  `GetTelemetry`.

- [ ] **7. Finish.** Mark the steps above. If everything passes, the branch is
  ready for a PR to `develop` (Squash and Merge).
