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

// TestHugeMemThresholdStillPausesByUtilization checks that a huge
// GPUMemThresholdMiB (reset to the default by NewEvaluator) never stops the
// primary GPU utilization rule (default 45%) from pausing.
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

// TestMaxUtilThresholdPlusHugeMemStillPauses pins the R3 fix: with
// GPUUtilThresholdPct=100 and an unbounded GPUMemThresholdMiB, 99% utilization
// with 16 GiB in use used to never pause. NewEvaluator now resets any memory
// threshold above 64 GiB to the default, so the memory rule pauses again.
func TestMaxUtilThresholdPlusHugeMemStillPauses(t *testing.T) {
	cfg := Config{
		AllowBatteryFolding:    true,
		GPUUtilThresholdPct:    100,
		GPUMemThresholdMiB:     math.MaxInt,
		GPUMemRequiresUtilPct:  25,
		SustainedTicksRequired: 3,
		MinBatteryPct:          50,
	}
	ev := NewEvaluator(cfg)
	if got, want := ev.cfg.GPUMemThresholdMiB, DefaultConfig().GPUMemThresholdMiB; got != want {
		t.Fatalf("GPUMemThresholdMiB = %d, se esperaba %d", got, want)
	}

	snap := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 99, GPUInUseMemMiB: 16 * 1024}
	for i := 0; i < cfg.SustainedTicksRequired-1; i++ {
		ev.Evaluate(snap)
	}
	if got := ev.Evaluate(snap); got != StatePausedLoad {
		t.Fatalf("estado = %q, se esperaba %q", got, StatePausedLoad)
	}
}

// TestMemThresholdUpperBound checks the 64 GiB bound: the limit itself is
// kept and anything above it falls back to the default.
func TestMemThresholdUpperBound(t *testing.T) {
	cases := map[int]int{
		maxGPUMemThresholdMiB:     maxGPUMemThresholdMiB,
		maxGPUMemThresholdMiB + 1: DefaultConfig().GPUMemThresholdMiB,
		math.MaxInt:               DefaultConfig().GPUMemThresholdMiB,
	}
	for in, want := range cases {
		cfg := DefaultConfig()
		cfg.GPUMemThresholdMiB = in
		if got := NewEvaluator(cfg).cfg.GPUMemThresholdMiB; got != want {
			t.Errorf("GPUMemThresholdMiB(%d) = %d, se esperaba %d", in, got, want)
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
