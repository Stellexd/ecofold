// Intentionally has no build tag: the cooldown and per-process load logic live
// in Evaluate and must be validated on both Linux and macOS.
package detector

import (
	"math"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// pauseFor drives the evaluator into StatePausedLoad and fails the test if it
// does not get there.
func pauseFor(t *testing.T, ev *Evaluator, snap SystemSnapshot) {
	t.Helper()
	for i := 0; i < ev.cfg.SustainedTicksRequired; i++ {
		ev.Evaluate(snap)
	}
	if got := ev.Evaluate(snap); got != StatePausedLoad {
		t.Fatalf("no se alcanzó %q: estado = %q", StatePausedLoad, got)
	}
}

// TestCooldownConfigIsBounded checks the documented clamp: the cooldown can
// never be disabled and never exceeds three minutes at a 2 s tick.
func TestCooldownConfigIsBounded(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{in: math.MinInt, want: 60},
		{in: -1, want: 60},
		{in: 0, want: 60},
		{in: 1, want: 60},
		{in: 59, want: 60},
		{in: 60, want: 60},
		{in: 75, want: 75},
		{in: 90, want: 90},
		{in: 91, want: 90},
		{in: math.MaxInt, want: 90},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		cfg.CooldownTicksRequired = c.in
		if got := NewEvaluator(cfg).cfg.CooldownTicksRequired; got != c.want {
			t.Errorf("CooldownTicksRequired(%d) = %d, se esperaba %d", c.in, got, c.want)
		}
	}
}

// TestCooldownHoldsPauseForFullWindow verifies that folding keeps paused for
// exactly CooldownTicksRequired load-free ticks and resumes on the next one.
func TestCooldownHoldsPauseForFullWindow(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	game := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 90}
	idle := idleDesktopSnapshot()

	pauseFor(t, ev, game)

	for i := 1; i < cfg.CooldownTicksRequired; i++ {
		if got := ev.Evaluate(idle); got != StatePausedLoad {
			t.Fatalf("tick %d/%d sin carga: estado = %q, se esperaba %q",
				i, cfg.CooldownTicksRequired, got, StatePausedLoad)
		}
	}
	if got := ev.Evaluate(idle); got != StateFolding {
		t.Fatalf("tick %d sin carga: estado = %q, se esperaba %q",
			cfg.CooldownTicksRequired, got, StateFolding)
	}
}

// TestCooldownRestartsOnIntermittentLoad verifies that a single load tick
// during the cooldown restarts it in full, even if it does not reach the
// sustained-load streak again.
func TestCooldownRestartsOnIntermittentLoad(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	game := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 90}
	idle := idleDesktopSnapshot()

	pauseFor(t, ev, game)
	for i := 0; i < cfg.CooldownTicksRequired-1; i++ {
		ev.Evaluate(idle)
	}

	if got := ev.Evaluate(game); got != StatePausedLoad {
		t.Fatalf("pulso de carga durante el cooldown: estado = %q, se esperaba %q", got, StatePausedLoad)
	}
	for i := 1; i < cfg.CooldownTicksRequired; i++ {
		if got := ev.Evaluate(idle); got != StatePausedLoad {
			t.Fatalf("tick %d/%d tras el pulso: estado = %q, se esperaba %q (cooldown reiniciado)",
				i, cfg.CooldownTicksRequired, got, StatePausedLoad)
		}
	}
	if got := ev.Evaluate(idle); got != StateFolding {
		t.Fatalf("tras el cooldown reiniciado: estado = %q, se esperaba %q", got, StateFolding)
	}
}

// TestBatteryPauseIsNotDelayedByCooldown verifies that the cooldown applies to
// the load latch only: a low battery pauses immediately and recovers
// immediately when the battery is healthy again.
func TestBatteryPauseIsNotDelayedByCooldown(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())

	lowBattery := SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 10}
	if got := ev.Evaluate(lowBattery); got != StatePausedBattery {
		t.Fatalf("batería al 10%%: estado = %q, se esperaba %q", got, StatePausedBattery)
	}
	healthy := SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 80}
	if got := ev.Evaluate(healthy); got != StateFoldingEco {
		t.Fatalf("batería al 80%%: estado = %q, se esperaba %q (sin cooldown de batería)",
			got, StateFoldingEco)
	}
}

// TestPerProcessLoadTriggersPause covers the Linux per-process rule: the
// aggregate device fields stay at zero, and one process with enough utilization
// must pause folding.
func TestPerProcessLoadTriggersPause(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	snap := SystemSnapshot{
		OnAC: true,
		GPUProcesses: []gpu.ProcessInfo{
			{PID: 1, UsedVRAMMiB: 800, SMUtilPct: 90, SMUtilValid: true},
		},
	}
	pauseFor(t, ev, snap)
}

// TestPerProcessMemoryRequiresItsOwnUtilization verifies the memory heuristic
// is evaluated inside a single PID: a process with high VRAM but low SM does
// not pause, and VRAM from one process cannot be combined with another
// process's utilization.
func TestPerProcessMemoryRequiresItsOwnUtilization(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	snap := SystemSnapshot{
		OnAC: true,
		GPUProcesses: []gpu.ProcessInfo{
			{PID: 1, UsedVRAMMiB: 4096, SMUtilPct: 10, SMUtilValid: true},
			{PID: 2, UsedVRAMMiB: 10, SMUtilPct: 30, SMUtilValid: true},
		},
	}
	for i := 0; i < cfg.SustainedTicksRequired+5; i++ {
		if got := ev.Evaluate(snap); got == StatePausedLoad {
			t.Fatalf("tick %d: se combinó VRAM de un PID con SM de otro (estado %q)", i+1, got)
		}
	}

	// The same process passing both conditions does pause.
	combined := SystemSnapshot{
		OnAC: true,
		GPUProcesses: []gpu.ProcessInfo{
			{PID: 1, UsedVRAMMiB: 4096, SMUtilPct: 30, SMUtilValid: true},
		},
	}
	for i := 0; i < cfg.SustainedTicksRequired; i++ {
		ev.Evaluate(combined)
	}
	if got := ev.Evaluate(combined); got != StatePausedLoad {
		t.Fatalf("VRAM y SM del mismo PID: estado = %q, se esperaba %q", got, StatePausedLoad)
	}
}

// TestUnsupportedUtilizationIsNotInactivity verifies that SMUtilValid=false is
// treated as "unknown" and never as an idle measurement: it cannot trigger a
// pause, but it also cannot reset an existing one.
func TestUnsupportedUtilizationIsNotInactivity(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	unknown := SystemSnapshot{
		OnAC: true,
		GPUProcesses: []gpu.ProcessInfo{
			{PID: 1, UsedVRAMMiB: 8192, SMUtilPct: 100, SMUtilValid: false},
		},
	}
	for i := 0; i < cfg.SustainedTicksRequired+5; i++ {
		if got := ev.Evaluate(unknown); got == StatePausedLoad {
			t.Fatalf("tick %d: SMUtilValid=false contó como carga", i+1)
		}
	}

	// An invalid sample must not release a pause established by real load.
	pauseFor(t, ev, SystemSnapshot{
		OnAC:         true,
		GPUProcesses: []gpu.ProcessInfo{{PID: 1, SMUtilPct: 90, SMUtilValid: true}},
	})
	if got := ev.Evaluate(unknown); got != StatePausedLoad {
		t.Fatalf("SMUtilValid=false liberó la pausa: estado = %q", got)
	}
}

// TestGlobalMetricsStillDriveMacOSPath keeps the device-wide branch covered:
// when GPUProcesses is nil (macOS), aggregate utilization still pauses.
func TestGlobalMetricsStillDriveMacOSPath(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	snap := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 60, GPUInUseMemMiB: 2048}
	pauseFor(t, ev, snap)
}

// TestGlobalMetricsIgnoredWhenGPUProcessesPresent verifies that a non-nil
// GPUProcesses slice disables the device-wide path. This prevents a collector
// that also copies aggregate telemetry from pausing folding permanently while
// only Folding@home is running.
func TestGlobalMetricsIgnoredWhenGPUProcessesPresent(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	snap := SystemSnapshot{
		OnAC:             true,
		GPUDeviceUtilPct: 99,
		GPUInUseMemMiB:   4096,
		GPUProcesses:     []gpu.ProcessInfo{}, // non-nil but empty
	}
	for i := 0; i < ev.cfg.SustainedTicksRequired+5; i++ {
		if got := ev.Evaluate(snap); got == StatePausedLoad {
			t.Fatalf("tick %d: pausó con métricas globales a pesar de GPUProcesses no nil", i+1)
		}
	}
}

// TestBatteryPauseTakesPrecedenceDuringCooldown verifies that if the battery
// demands a pause while the load cooldown is active, the evaluator reports the
// battery reason immediately, then returns to the load cooldown once the
// battery is healthy.
func TestBatteryPauseTakesPrecedenceDuringCooldown(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	game := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 90}
	pauseFor(t, ev, game)

	// Battery becomes low during the cooldown: StatePausedBattery immediately.
	lowBattery := SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 10, GPUProcesses: []gpu.ProcessInfo{}}
	if got := ev.Evaluate(lowBattery); got != StatePausedBattery {
		t.Fatalf("batería baja durante cooldown: estado = %q, se esperaba %q", got, StatePausedBattery)
	}

	// Battery recovers but load cooldown is still counting. The low-battery
	// tick counted as the first load-free tick, so CooldownTicksRequired-2
	// additional ticks remain paused before folding resumes.
	healthy := SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 80, GPUProcesses: []gpu.ProcessInfo{}}
	pausedTicksAfterBattery := cfg.CooldownTicksRequired - 2
	for i := 1; i <= pausedTicksAfterBattery; i++ {
		if got := ev.Evaluate(healthy); got != StatePausedLoad {
			t.Fatalf("healthy tick %d/%d tras recuperar batería: estado = %q, se esperaba %q",
				i, pausedTicksAfterBattery, got, StatePausedLoad)
		}
	}
	if got := ev.Evaluate(healthy); got != StateFoldingEco {
		t.Fatalf("tras cooldown y batería sana: estado = %q, se esperaba %q", got, StateFoldingEco)
	}
}
