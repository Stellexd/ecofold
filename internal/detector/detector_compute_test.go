package detector

import (
	"math"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// blenderRenderSM is the SM series measured on CachyOS for Blender Cycles
// (OptiX) rendering next to FahCore at 61-98 %, one sample per 2 s tick.
var blenderRenderSM = []int{0, 10, 0, 0, 20, 0, 0, 0, 7, 10}

func computeSnap(pid uint32, smPct int, vramMiB uint64, compute bool) SystemSnapshot {
	return SystemSnapshot{
		OnAC:         true,
		GPUProcesses: []gpu.ProcessInfo{{PID: pid, UsedVRAMMiB: vramMiB, SMUtilPct: smPct, SMUtilValid: true, Compute: compute}},
	}
}

// TestComputeContentionPausesBlenderRender: the measured render must pause
// folding even though Blender never reaches 45 % SM or 1500 MiB.
func TestComputeContentionPausesBlenderRender(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	paused := -1
	for i, sm := range blenderRenderSM {
		if ev.Evaluate(computeSnap(330989, sm, 1424, true)) == StatePausedLoad && paused < 0 {
			paused = i
		}
	}
	if paused < 0 {
		t.Fatal("el render de Blender no pausó el plegado")
	}
	// First sample above 0 at tick 1, plus SustainedTicksRequired-1 ticks.
	if want := 1 + DefaultConfig().SustainedTicksRequired - 1; paused != want {
		t.Fatalf("pausa en el tick %d, se esperaba %d", paused, want)
	}
}

// TestIdleBlenderKeepsFolding: Blender at rest keeps a 437 MiB CUDA context
// with 0 % SM; it must not pause folding.
func TestIdleBlenderKeepsFolding(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	for i := 0; i < 200; i++ {
		if got := ev.Evaluate(computeSnap(330989, 0, 437, true)); got != StateFolding {
			t.Fatalf("tick %d = %q, se esperaba %q", i, got, StateFolding)
		}
	}
}

// TestComputeRuleBoundaries covers what the contention rule must not catch.
func TestComputeRuleBoundaries(t *testing.T) {
	cases := map[string]SystemSnapshot{
		"small context (Discord renderer)": computeSnap(1, 15, 177, true),
		"graphics process below mem rule":  computeSnap(2, 15, 1424, false),
	}
	for name, snap := range cases {
		t.Run(name, func(t *testing.T) {
			ev := NewEvaluator(DefaultConfig())
			for i := 0; i < 20; i++ {
				if got := ev.Evaluate(snap); got != StateFolding {
					t.Fatalf("tick %d = %q, se esperaba %q", i, got, StateFolding)
				}
			}
		})
	}
}

// TestComputeActivityWindowExpires: once the render ends (SM stays at 0), the
// process stops contending after computeActivityTicks and the cooldown runs.
func TestComputeActivityWindowExpires(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	for i := 0; i < 5; i++ {
		ev.Evaluate(computeSnap(7, 30, 1024, true))
	}
	idle := computeSnap(7, 0, 437, true)
	idle.GPUProcesses[0].UsedVRAMMiB = 1024
	var got OperatingState
	for i := 0; i < computeActivityTicks-1+DefaultConfig().CooldownTicksRequired; i++ {
		got = ev.Evaluate(idle)
	}
	if got != StateFolding {
		t.Fatalf("tras la ventana y el cooldown = %q, se esperaba %q", got, StateFolding)
	}
}

// TestComputeSeenIsPruned: PIDs that leave the GPU are forgotten, so a reused
// PID does not inherit activity.
func TestComputeSeenIsPruned(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	ev.Evaluate(computeSnap(9, 50, 1024, true))
	ev.Evaluate(SystemSnapshot{OnAC: true, GPUProcesses: []gpu.ProcessInfo{}})
	if _, ok := ev.computeSeen[9]; ok {
		t.Fatal("computeSeen conserva un PID que ya no está en la GPU")
	}
}

func TestComputeMemThresholdClamp(t *testing.T) {
	want := DefaultConfig().ComputeMemThresholdMiB
	for _, v := range []int{-1, 0, maxGPUMemThresholdMiB + 1, math.MaxInt} {
		cfg := DefaultConfig()
		cfg.ComputeMemThresholdMiB = v
		if got := NewEvaluator(cfg).cfg.ComputeMemThresholdMiB; got != want {
			t.Errorf("ComputeMemThresholdMiB(%d) = %d, se esperaba %d", v, got, want)
		}
	}
}

// TestUserPauseListPausesRegardlessOfGPU: a match pauses after the usual
// sustain, holds while the app runs and releases after the cooldown, on both
// the per-process (Linux) and global (macOS) paths.
func TestUserPauseListPausesRegardlessOfGPU(t *testing.T) {
	for name, base := range map[string]SystemSnapshot{
		"linux": {OnAC: true, GPUProcesses: []gpu.ProcessInfo{}},
		"macos": {OnAC: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			ev := NewEvaluator(cfg)
			open := base
			open.UserPauseMatches = []string{"obs"}
			var got OperatingState
			for i := 0; i < cfg.SustainedTicksRequired; i++ {
				got = ev.Evaluate(open)
			}
			if got != StatePausedLoad {
				t.Fatalf("con la app abierta = %q, se esperaba %q", got, StatePausedLoad)
			}
			for i := 0; i < 3*cfg.CooldownTicksRequired; i++ {
				if got := ev.Evaluate(open); got != StatePausedLoad {
					t.Fatalf("tick %d con la app abierta = %q", i, got)
				}
			}
			for i := 0; i < cfg.CooldownTicksRequired; i++ {
				got = ev.Evaluate(base)
			}
			if got != StateFolding {
				t.Fatalf("tras cerrar la app y el cooldown = %q, se esperaba %q", got, StateFolding)
			}
		})
	}
}
