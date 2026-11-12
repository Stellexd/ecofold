package detector

import "testing"

// Hardening fuzzers from the m1 chaos pass (docs/CHAOS_REPORT.md).

// FuzzNewEvaluatorConfigSpace explores the whole Config space and checks the
// clamp invariants plus two safety properties: sustained 100% utilization
// always pauses within SustainedTicksRequired ticks (no fail-open), and an
// idle machine on AC never pauses.
func FuzzNewEvaluatorConfigSpace(f *testing.F) {
	f.Add(0, false, 0, 0, 0, 0)
	f.Add(50, true, 45, 1500, 25, 3)
	f.Add(-1, true, 100, 1<<62, 100, 60)
	f.Add(101, false, 26, -1, 24, 61)
	f.Add(100, true, 25, 0, 101, -1)
	f.Fuzz(func(t *testing.T, minBat int, allowBat bool, util, mem, memReq, ticks int) {
		cfg := Config{
			MinBatteryPct: minBat, AllowBatteryFolding: allowBat, GPUUtilThresholdPct: util,
			GPUMemThresholdMiB: mem, GPUMemRequiresUtilPct: memReq, SustainedTicksRequired: ticks,
		}
		ev := NewEvaluator(cfg)
		c := ev.cfg
		switch {
		case c.GPUUtilThresholdPct <= 25 || c.GPUUtilThresholdPct > 100:
			t.Fatalf("util threshold %d out of (25,100]", c.GPUUtilThresholdPct)
		case c.GPUMemThresholdMiB <= 0:
			t.Fatalf("mem threshold %d <= 0", c.GPUMemThresholdMiB)
		case c.SustainedTicksRequired < 1 || c.SustainedTicksRequired > maxSustainedTicksRequired:
			t.Fatalf("ticks %d out of [1,60]", c.SustainedTicksRequired)
		case c.GPUMemRequiresUtilPct < 25 || c.GPUMemRequiresUtilPct > c.GPUUtilThresholdPct:
			t.Fatalf("mem-requires-util %d out of [25,%d]", c.GPUMemRequiresUtilPct, c.GPUUtilThresholdPct)
		case c.MinBatteryPct < 0 || c.MinBatteryPct > 100:
			t.Fatalf("min battery %d out of [0,100]", c.MinBatteryPct)
		}

		idle := SystemSnapshot{OnAC: true, BatteryPct: 100}
		for i := range maxSustainedTicksRequired + 1 {
			if got := ev.Evaluate(idle); got != StateFolding {
				t.Fatalf("idle on AC tick %d = %q, want folding", i, got)
			}
		}
		game := SystemSnapshot{HasBattery: true, BatteryPct: 1, GPUDeviceUtilPct: 100}
		paused := false
		for range c.SustainedTicksRequired {
			paused = ev.Evaluate(game) == StatePausedLoad
		}
		if !paused {
			t.Fatalf("100%% GPU for %d ticks did not pause (cfg %+v)", c.SustainedTicksRequired, c)
		}
	})
}
