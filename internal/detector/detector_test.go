// Intentionally has no build tag: Evaluate's logic is platform-agnostic and
// must be validated on both Linux and macOS. This file must not reference
// symbols exclusive to darwin_ioreg.go or linux_stub.go.
package detector

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

// evaluateTicks runs the evaluator n times on the same snapshot and returns the
// sequence of observed states (index 0 = tick 1).
func evaluateTicks(ev *Evaluator, snap SystemSnapshot, ticks int) []OperatingState {
	states := make([]OperatingState, 0, ticks)
	for i := 0; i < ticks; i++ {
		states = append(states, ev.Evaluate(snap))
	}
	return states
}

// assertAllStates requires every tick to return want, including the tick number
// in the error message to facilitate diagnosis.
func assertAllStates(t *testing.T, states []OperatingState, want OperatingState, what string) {
	t.Helper()
	for i, got := range states {
		if got != want {
			t.Errorf("%s: tick %d devolvió %q, se esperaba %q", what, i+1, got, want)
		}
	}
}

// idleDesktopSnapshot represents a powered-on desktop without a battery, with
// the GPU completely idle (0% utilization and 0 MiB reserved).
func idleDesktopSnapshot() SystemSnapshot {
	return SystemSnapshot{
		HasBattery:         false,
		OnAC:               true,
		GPUDeviceUtilPct:   0,
		GPURendererUtilPct: 0,
		GPUTilerUtilPct:    0,
		GPUInUseMemMiB:     0,
	}
}

// TestZeroConfigDoesNotPauseOnIdle verifies that Config{} (all fields at their
// zero values) does not pause folding while idle. NewEvaluator's clamps must
// fill in thresholds without inventing load where none exists.
func TestZeroConfigDoesNotPauseOnIdle(t *testing.T) {
	ev := NewEvaluator(Config{})
	snap := idleDesktopSnapshot()

	states := evaluateTicks(ev, snap, 5)
	assertAllStates(t, states, StateFolding, "Config{} en reposo")
}

// TestHighUnifiedMemoryAtZeroGPUUtilDoesNotPause covers the unified-memory
// false positive (Apple Silicon): 1600 MiB in use with the GPU at 0% is not
// game load because it does not meet GPUMemRequiresUtilPct (25%).
func TestHighUnifiedMemoryAtZeroGPUUtilDoesNotPause(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	snap := SystemSnapshot{
		HasBattery:       false,
		OnAC:             true,
		GPUClass:         "AGXAcceleratorG13X",
		GPUDeviceUtilPct: 0,
		GPUInUseMemMiB:   1600,
	}

	states := evaluateTicks(ev, snap, 5)
	assertAllStates(t, states, StateFolding, "memoria alta con GPU al 0%")
}

// TestHighMemoryWithModerateGPUPausesAfterSustainedTicks validates that the
// memory+utilization rule triggers a pause, but only after the required
// sustained ticks (3 by default).
func TestHighMemoryWithModerateGPUPausesAfterSustainedTicks(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	snap := SystemSnapshot{
		HasBattery:       false,
		OnAC:             true,
		GPUDeviceUtilPct: 30,
		GPUInUseMemMiB:   1600,
	}

	want := []OperatingState{
		StateFolding,    // tick 1: streak = 1
		StateFolding,    // tick 2: streak = 2
		StatePausedLoad, // tick 3: streak = 3
		StatePausedLoad, // tick 4: remains paused
		StatePausedLoad, // tick 5: remains paused
	}

	states := evaluateTicks(ev, snap, len(want))
	for i := range want {
		if states[i] != want[i] {
			t.Errorf("tick %d: Evaluate = %q, se esperaba %q", i+1, states[i], want[i])
		}
	}
}

// TestHighMemoryBelowRequiredUtilDoesNotPause verifies the lower boundary for
// GPUMemRequiresUtilPct: 24% (one point below the minimum of 25) with memory
// above the threshold must never count as load, even after 10 consecutive ticks.
func TestHighMemoryBelowRequiredUtilDoesNotPause(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	snap := SystemSnapshot{
		HasBattery:       false,
		OnAC:             true,
		GPUDeviceUtilPct: 24,
		GPUInUseMemMiB:   1600,
	}

	states := evaluateTicks(ev, snap, 10)
	for i, got := range states {
		if got == StatePausedLoad {
			t.Fatalf("tick %d: Evaluate = %q, se esperaba %q o %q (GPU al 24%% no debe contar como carga)",
				i+1, got, StateFolding, StateFoldingEco)
		}
	}
	assertAllStates(t, states, StateFolding, "memoria alta con GPU al 24%")
}

// TestStreakResetsWhenLoadDrops confirms that a single idle tick resets the
// sustained-load counter and prevents accumulating non-consecutive ticks.
func TestStreakResetsWhenLoadDrops(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())

	high := SystemSnapshot{
		HasBattery:       false,
		OnAC:             true,
		GPUDeviceUtilPct: 60,
	}
	idle := idleDesktopSnapshot()

	sequence := []SystemSnapshot{high, high, idle, high, high}
	for i, snap := range sequence {
		if got := ev.Evaluate(snap); got == StatePausedLoad {
			t.Fatalf("tick %d: Evaluate = %q, la racha no se reinició tras un tick de reposo", i+1, got)
		}
	}
}

// TestMinBatteryZeroIsPreserved guarantees that MinBatteryPct=0 is not replaced
// by the default value (50): with 0% configured, any battery level >= 0 must
// allow StateFoldingEco.
func TestMinBatteryZeroIsPreserved(t *testing.T) {
	cfg := Config{MinBatteryPct: 0, AllowBatteryFolding: true}

	cases := []struct {
		name       string
		batteryPct int
		want       OperatingState
	}{
		{name: "batería al 5%", batteryPct: 5, want: StateFoldingEco},
		{name: "batería al 0% (borde inferior)", batteryPct: 0, want: StateFoldingEco},
		{name: "batería negativa (lectura corrupta)", batteryPct: -1, want: StatePausedBattery},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvaluator(cfg)
			snap := SystemSnapshot{
				HasBattery:       true,
				OnAC:             false,
				BatteryPct:       tc.batteryPct,
				GPUDeviceUtilPct: 0,
				GPUInUseMemMiB:   0,
			}
			if got := ev.Evaluate(snap); got != tc.want {
				t.Fatalf("Evaluate(battery=%d) = %q, se esperaba %q", tc.batteryPct, got, tc.want)
			}
		})
	}
}

// TestEvaluateBoundaries groups the boundaries of the three thresholds: exact
// equality, one point below, negative values, and absurd overflow values. All
// cases use DefaultConfig with a new evaluator and 3 ticks (the sustained
// minimum) so that StatePausedLoad can be reached.
func TestEvaluateBoundaries(t *testing.T) {
	cases := []struct {
		name string
		snap SystemSnapshot
		want OperatingState
	}{
		{
			name: "memoria justo en el umbral y util justo en el mínimo",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 25, GPUInUseMemMiB: 1500},
			want: StatePausedLoad,
		},
		{
			name: "memoria un MiB por debajo del umbral",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 25, GPUInUseMemMiB: 1499},
			want: StateFolding,
		},
		{
			name: "util un punto por debajo del mínimo de memoria",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 24, GPUInUseMemMiB: 999999},
			want: StateFolding,
		},
		{
			name: "util justo en el umbral principal (45%)",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 45, GPUInUseMemMiB: 0},
			want: StatePausedLoad,
		},
		{
			name: "util un punto por debajo del umbral principal y sin memoria",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 44, GPUInUseMemMiB: 0},
			want: StateFolding,
		},
		{
			name: "valores negativos (lectura corrupta de NVML/IORegistry)",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: -1, GPUInUseMemMiB: -1},
			want: StateFolding,
		},
		{
			name: "utilización >100% y memoria desbordada",
			snap: SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 101, GPUInUseMemMiB: int(^uint(0) >> 1)},
			want: StatePausedLoad,
		},
		{
			name: "carga alta gana a la batería baja (prioridad documentada)",
			snap: SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 3, GPUDeviceUtilPct: 100},
			want: StatePausedLoad,
		},
		{
			name: "batería baja pero enchufado no pausa",
			snap: SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 3, GPUDeviceUtilPct: 0},
			want: StateFolding,
		},
		{
			name: "sin batería y sin CA se asume plegado normal",
			snap: SystemSnapshot{HasBattery: false, OnAC: false, BatteryPct: 0, GPUDeviceUtilPct: 0},
			want: StateFolding,
		},
		{
			name: "batería al 100% a batería permite plegado eco",
			snap: SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 100, GPUDeviceUtilPct: 0},
			want: StateFoldingEco, // DefaultConfig has AllowBatteryFolding=true
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvaluator(DefaultConfig())
			states := evaluateTicks(ev, tc.snap, 3)
			if got := states[len(states)-1]; got != tc.want {
				t.Fatalf("tick 3: Evaluate = %q, se esperaba %q", got, tc.want)
			}
		})
	}
}

// TestEvaluatorInstancesAreIndependentInParallel verifies that separate
// evaluators do not share state (the real use case: one evaluator per daemon
// connection/client). Run with -race.
func TestEvaluatorInstancesAreIndependentInParallel(t *testing.T) {
	const workers = 16
	const ticks = 50

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	for i := 0; i < workers; i++ {
		// High-load worker: it must finish paused.
		go func() {
			defer wg.Done()
			ev := NewEvaluator(DefaultConfig())
			snap := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 100}
			for j := 0; j < ticks; j++ {
				_ = ev.Evaluate(snap)
			}
			if got := ev.Evaluate(snap); got != StatePausedLoad {
				t.Errorf("evaluador con carga: último estado = %q, se esperaba %q", got, StatePausedLoad)
			}
		}()

		// Idle worker: it must never pause.
		go func() {
			defer wg.Done()
			ev := NewEvaluator(DefaultConfig())
			snap := idleDesktopSnapshot()
			for j := 0; j < ticks; j++ {
				if got := ev.Evaluate(snap); got != StateFolding {
					t.Errorf("evaluador en reposo: estado = %q, se esperaba %q", got, StateFolding)
					return
				}
			}
		}()
	}

	wg.Wait()
}

// TestSharedEvaluatorConcurrentAccess hammers a single Evaluator instance from
// 32 goroutines, each emitting 100 ticks that alternate between sustained
// high-load and idle snapshots. Evaluate must be safe for concurrent use (the
// daemon may share one evaluator across clients) and every returned state must
// belong to the closed set. Run with -race.
func TestSharedEvaluatorConcurrentAccess(t *testing.T) {
	const workers = 32
	const ticks = 100

	valid := map[OperatingState]bool{
		StateFolding:       true,
		StateFoldingEco:    true,
		StatePausedBattery: true,
		StatePausedLoad:    true,
	}

	ev := NewEvaluator(DefaultConfig())

	highLoad := SystemSnapshot{
		HasBattery:       false,
		OnAC:             true,
		GPUClass:         "AGXAcceleratorG16G",
		GPUDeviceUtilPct: 60,
		GPUInUseMemMiB:   2048,
	}
	idle := idleDesktopSnapshot()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < ticks; i++ {
				snap := idle
				if (w+i)%2 == 0 {
					snap = highLoad
				}
				got := ev.Evaluate(snap)
				if !valid[got] {
					t.Errorf("goroutine %d, tick %d: Evaluate devolvió un estado fuera del conjunto cerrado: %q", w, i+1, got)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestHighLoadStreakIntegerOverflow drives a single evaluator through 100000
// consecutive high-load ticks (GPU at 60%, above the 45% threshold). The streak
// must saturate at SustainedTicksRequired instead of growing without bound
// (which would eventually overflow into negative territory and re-open the
// pause decision), and the state must remain StatePausedLoad.
func TestHighLoadStreakIntegerOverflow(t *testing.T) {
	const ticks = 100000

	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	highLoad := SystemSnapshot{
		HasBattery:       false,
		OnAC:             true,
		GPUDeviceUtilPct: 60,
	}

	for i := 0; i < ticks; i++ {
		got := ev.Evaluate(highLoad)
		// Ticks 1 and 2 only build the streak; from tick 3 onwards the pause must
		// be reported on every single tick.
		if i >= cfg.SustainedTicksRequired-1 && got != StatePausedLoad {
			t.Fatalf("tick %d: Evaluate = %q, se esperaba %q", i+1, got, StatePausedLoad)
		}
	}

	ev.mu.Lock()
	streak := ev.highLoadStreak
	ev.mu.Unlock()

	if streak > cfg.SustainedTicksRequired {
		t.Fatalf("highLoadStreak = %d tras %d ticks, debe saturarse en <= %d (riesgo de desbordamiento)",
			streak, ticks, cfg.SustainedTicksRequired)
	}
	if streak != cfg.SustainedTicksRequired {
		t.Errorf("highLoadStreak = %d tras %d ticks, se esperaba exactamente %d",
			streak, ticks, cfg.SustainedTicksRequired)
	}

	// Regression: the first idle tick must reset the high-load streak but keep
	// the pause latched for the cooldown. Folding only resumes after
	// CooldownTicksRequired consecutive load-free ticks.
	idle := idleDesktopSnapshot()
	if got := ev.Evaluate(idle); got != StatePausedLoad {
		t.Fatalf("primer tick en reposo tras %d ticks de carga: Evaluate = %q, se esperaba %q (cooldown)",
			ticks, got, StatePausedLoad)
	}

	ev.mu.Lock()
	streakAfterIdle := ev.highLoadStreak
	ev.mu.Unlock()
	if streakAfterIdle != 0 {
		t.Errorf("highLoadStreak = %d tras el tick en reposo, se esperaba 0", streakAfterIdle)
	}

	for i := 0; i < cfg.CooldownTicksRequired-2; i++ {
		if got := ev.Evaluate(idle); got != StatePausedLoad {
			t.Fatalf("tick %d/%d del cooldown: Evaluate = %q, se esperaba %q",
				i+2, cfg.CooldownTicksRequired, got, StatePausedLoad)
		}
	}
	if got := ev.Evaluate(idle); got != StateFolding {
		t.Fatalf("tras %d ticks de cooldown: Evaluate = %q, se esperaba %q",
			cfg.CooldownTicksRequired, got, StateFolding)
	}
}

// FuzzEvaluateNeverPanics subjects Evaluate to arbitrary telemetry combinations
// (including negative and extreme values). Any panic is a failure; the returned
// state must belong to the known set.
func FuzzEvaluateNeverPanics(f *testing.F) {
	valid := map[OperatingState]bool{
		StateFolding:       true,
		StateFoldingEco:    true,
		StatePausedBattery: true,
		StatePausedLoad:    true,
	}

	f.Add(false, true, 0, 0, 0, 0)
	f.Add(true, false, 5, 0, 0, 0)
	f.Add(true, false, 49, 30, 1600, 1500)
	f.Add(true, true, 100, 101, 1<<20, 0)
	f.Add(false, false, -1, -1, -1, -1)

	f.Fuzz(func(t *testing.T, hasBattery, onAC bool, batteryPct, utilPct, memMiB, memThreshold int) {
		cfg := DefaultConfig()
		cfg.GPUMemThresholdMiB = memThreshold
		ev := NewEvaluator(cfg)
		_ = ev.Evaluate(SystemSnapshot{
			HasBattery:       hasBattery,
			OnAC:             onAC,
			BatteryPct:       batteryPct,
			GPUDeviceUtilPct: utilPct,
			GPUInUseMemMiB:   memMiB,
		})

		// The contract is that Evaluate never panics and returns a state from the
		// closed set.
		got := ev.Evaluate(SystemSnapshot{})
		if !valid[got] {
			t.Fatalf("Evaluate devolvió un estado desconocido: %q", got)
		}
	})
}

// TestGPUMemRequiresUtilPctIsClampedTo25 verifies that values below the
// safety floor (including 1) are clamped to 25.
func TestGPUMemRequiresUtilPctIsClampedTo25(t *testing.T) {
	ev := NewEvaluator(Config{GPUUtilThresholdPct: 45, GPUMemRequiresUtilPct: 1})
	if ev.cfg.GPUMemRequiresUtilPct != 25 {
		t.Errorf("GPUMemRequiresUtilPct = %d, se esperaba 25", ev.cfg.GPUMemRequiresUtilPct)
	}
}

// TestConfigExtremesAreClampedAndEvaluable covers absurd or hostile
// configurations (negative, zero) to confirm that NewEvaluator never produces
// an unusable evaluator or panics.
func TestConfigExtremesAreClampedAndEvaluable(t *testing.T) {
	configs := []Config{
		{},
		{GPUUtilThresholdPct: -1, GPUMemThresholdMiB: -1, SustainedTicksRequired: -1, GPUMemRequiresUtilPct: -1, MinBatteryPct: -1},
		{GPUUtilThresholdPct: 25, GPUMemThresholdMiB: 0, SustainedTicksRequired: 0, GPUMemRequiresUtilPct: 0},
		{GPUUtilThresholdPct: 1, SustainedTicksRequired: 1, GPUMemRequiresUtilPct: 1},
		{GPUUtilThresholdPct: 1000, GPUMemThresholdMiB: 1000, SustainedTicksRequired: 1000},
	}

	for i, cfg := range configs {
		t.Run(fmt.Sprintf("config_%d", i), func(t *testing.T) {
			ev := NewEvaluator(cfg)
			for _, snap := range []SystemSnapshot{
				{},
				{OnAC: true, GPUDeviceUtilPct: 100, GPUInUseMemMiB: 1 << 30},
				{HasBattery: true, OnAC: false, BatteryPct: 1},
			} {
				if got := ev.Evaluate(snap); got != StateFolding &&
					got != StateFoldingEco && got != StatePausedBattery && got != StatePausedLoad {
					t.Fatalf("estado fuera de contrato: %q", got)
				}
			}
		})
	}
}

// TestGPUUtilThresholdAbove100IsClamped verifies that an unreachable threshold
// is replaced by the default instead of disabling load pausing (M5).
func TestGPUUtilThresholdAbove100IsClamped(t *testing.T) {
	for _, pct := range []int{101, 1000, math.MaxInt} {
		ev := NewEvaluator(Config{GPUUtilThresholdPct: pct})
		if ev.cfg.GPUUtilThresholdPct != DefaultConfig().GPUUtilThresholdPct {
			t.Errorf("GPUUtilThresholdPct(%d) = %d, se esperaba %d", pct, ev.cfg.GPUUtilThresholdPct, DefaultConfig().GPUUtilThresholdPct)
		}

		var got OperatingState
		for range ev.cfg.SustainedTicksRequired {
			got = ev.Evaluate(SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 100})
		}
		if got != StatePausedLoad {
			t.Errorf("con GPUUtilThresholdPct=%d y 100%% de uso sostenido, estado = %q, se esperaba %q", pct, got, StatePausedLoad)
		}
	}

	ev := NewEvaluator(Config{GPUUtilThresholdPct: 100})
	if ev.cfg.GPUUtilThresholdPct != 100 {
		t.Errorf("GPUUtilThresholdPct(100) = %d, se esperaba 100 (límite válido)", ev.cfg.GPUUtilThresholdPct)
	}
}
