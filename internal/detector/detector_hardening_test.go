// Intentionally has no build tag: the config clamp logic is platform-agnostic.

package detector

import (
	"math"
	"testing"
)

// TestSustainedTicksHugeIsClamped reproduces a remaining fail-open in M5's
// clamp logic. A SustainedTicksRequired value so large that it is unreachable
// (for example a typo such as 1e9, or math.MaxInt) silently disables load
// pausing even with every other threshold at its default: the evaluator keeps
// folding at 100% GPU utilization forever.
//
// Expected safe behavior: NewEvaluator must bound SustainedTicksRequired to a
// sane maximum (or reset absurd values to the default), the same way it already
// rejects GPUUtilThresholdPct > 100 as unreachable.
func TestSustainedTicksHugeIsClamped(t *testing.T) {
	for _, ticks := range []int{1_000_000, math.MaxInt} {
		cfg := DefaultConfig()
		cfg.SustainedTicksRequired = ticks
		ev := NewEvaluator(cfg)

		if ev.cfg.SustainedTicksRequired > maxSustainedTicksRequired {
			t.Errorf("SustainedTicksRequired(%d) = %d, se esperaba un valor acotado a <= %d",
				ticks, ev.cfg.SustainedTicksRequired, maxSustainedTicksRequired)
		}

		highLoad := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 100, GPUInUseMemMiB: 4096}
		var got OperatingState
		for i := 0; i < maxSustainedTicksRequired+1; i++ {
			got = ev.Evaluate(highLoad)
		}
		if got != StatePausedLoad {
			t.Errorf("con SustainedTicksRequired=%d, tras %d ticks al 100%% de GPU el estado = %q, se esperaba %q (fail-open)",
				ticks, maxSustainedTicksRequired+1, got, StatePausedLoad)
		}
	}
}

// TestSustainedTicksWithinBoundStillWorks is the control case: a large but
// reachable value (60) must still trigger the pause after exactly that many
// ticks, proving the clamp would not break legitimate configuration.
func TestSustainedTicksWithinBoundStillWorks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SustainedTicksRequired = maxSustainedTicksRequired
	ev := NewEvaluator(cfg)

	highLoad := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 100}
	for i := 1; i < maxSustainedTicksRequired; i++ {
		if got := ev.Evaluate(highLoad); got == StatePausedLoad {
			t.Fatalf("tick %d: pausó antes de tiempo con umbral %d", i, maxSustainedTicksRequired)
		}
	}
	if got := ev.Evaluate(highLoad); got != StatePausedLoad {
		t.Fatalf("tick %d: estado = %q, se esperaba %q", maxSustainedTicksRequired, got, StatePausedLoad)
	}
}

// TestHugeMemThresholdStillPausesByUtilization documents that an unbounded
// GPUMemThresholdMiB only disables the memory heuristic; the primary GPU
// utilization rule (default 45%) still works. This is not a fail-open as long
// as GPUUtilThresholdPct is not itself set to 100.
func TestHugeMemThresholdStillPausesByUtilization(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GPUMemThresholdMiB = math.MaxInt
	ev := NewEvaluator(cfg)

	highLoad := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 50, GPUInUseMemMiB: math.MaxInt}
	for i := 0; i < cfg.SustainedTicksRequired; i++ {
		ev.Evaluate(highLoad)
	}
	if got := ev.Evaluate(highLoad); got != StatePausedLoad {
		t.Fatalf("estado = %q, se esperaba %q (la regla de utilización debe seguir activa)",
			got, StatePausedLoad)
	}
}

// TestMaxUtilThresholdPlusHugeMemDisablesPause documents the combination that
// slips through M5: GPUUtilThresholdPct=100 is considered valid by NewEvaluator
// and GPUMemThresholdMiB has no upper bound, so 99% utilization with 16 GiB in
// use never pauses. Both values are "in range"; only the combination is
// fail-open. Reported to the coder as a residual risk (lower severity than the
// SustainedTicks one because 100% utilization is reachable on real hardware).
func TestMaxUtilThresholdPlusHugeMemDisablesPause(t *testing.T) {
	cfg := Config{
		AllowBatteryFolding:    true,
		GPUUtilThresholdPct:    100,
		GPUMemThresholdMiB:     math.MaxInt,
		GPUMemRequiresUtilPct:  25,
		SustainedTicksRequired: 3,
		MinBatteryPct:          50,
	}
	ev := NewEvaluator(cfg)

	snap := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 99, GPUInUseMemMiB: 16 * 1024}
	for i := 0; i < 10; i++ {
		if got := ev.Evaluate(snap); got != StateFolding {
			t.Fatalf("tick %d: estado = %q, se esperaba %q (documentando el fail-open residual)",
				i+1, got, StateFolding)
		}
	}
}

// TestMinBatteryPctAboveHundredIsClamped checks that an unreachable battery
// threshold is reset to the default instead of pausing on battery forever.
func TestMinBatteryPctAboveHundredIsClamped(t *testing.T) {
	want := DefaultConfig().MinBatteryPct
	for _, pct := range []int{101, 1000, math.MaxInt} {
		cfg := DefaultConfig()
		cfg.MinBatteryPct = pct
		if got := NewEvaluator(cfg).cfg.MinBatteryPct; got != want {
			t.Errorf("MinBatteryPct(%d) = %d, se esperaba %d", pct, got, want)
		}
	}

	// 100 is a valid (if strict) threshold and must be preserved.
	cfg := DefaultConfig()
	cfg.MinBatteryPct = 100
	if got := NewEvaluator(cfg).cfg.MinBatteryPct; got != 100 {
		t.Errorf("MinBatteryPct(100) = %d, se esperaba 100", got)
	}
}
