// Intentionally has no build tag: the thermal latch lives in Evaluate and must
// be validated on both Linux and macOS.
package detector

import "testing"

// TestThermalPressurePausesAtOnce: serious (2) and critical (3, 4) pause on
// the first tick, nominal (0) and moderate (1) do not.
func TestThermalPressurePausesAtOnce(t *testing.T) {
	for level, want := range map[int]OperatingState{
		0: StateFolding, 1: StateFolding,
		2: StatePausedThermal, 3: StatePausedThermal, 4: StatePausedThermal,
	} {
		ev := NewEvaluator(DefaultConfig())
		snap := SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 90, ThermalLevel: level}
		if got := ev.Evaluate(snap); got != want {
			t.Errorf("nivel %d: estado = %q, se esperaba %q", level, got, want)
		}
	}
}

// TestThermalPauseHoldsForTheCooldown: once the pressure drops the pause
// lasts CooldownTicksRequired ticks, and a new serious tick restarts it.
func TestThermalPauseHoldsForTheCooldown(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	hot := SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 90, ThermalLevel: 2}
	cool := hot
	cool.ThermalLevel = 1

	if got := ev.Evaluate(hot); got != StatePausedThermal {
		t.Fatalf("caliente: %q", got)
	}
	for i := 1; i < cfg.CooldownTicksRequired; i++ {
		if got := ev.Evaluate(cool); got != StatePausedThermal {
			t.Fatalf("tick %d del enfriamiento: %q", i, got)
		}
	}
	// It heats up again one tick before the end: the cooldown starts over.
	if got := ev.Evaluate(hot); got != StatePausedThermal {
		t.Fatalf("recalentado: %q", got)
	}
	for i := 1; i < cfg.CooldownTicksRequired; i++ {
		if got := ev.Evaluate(cool); got != StatePausedThermal {
			t.Fatalf("tick %d del segundo enfriamiento: %q", i, got)
		}
	}
	if got := ev.Evaluate(cool); got != StateFoldingEco {
		t.Fatalf("tras el enfriamiento completo: %q", got)
	}
}

// TestThermalPauseOutranksBattery: a hot Mac on low battery reports the
// thermal reason, and the battery one once it has cooled down.
func TestThermalPauseOutranksBattery(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	snap := SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 10, ThermalLevel: 3}
	if got := ev.Evaluate(snap); got != StatePausedThermal {
		t.Fatalf("caliente con batería baja: %q", got)
	}
	snap.ThermalLevel = 0
	for i := 0; i < cfg.CooldownTicksRequired; i++ {
		ev.Evaluate(snap)
	}
	if got := ev.Evaluate(snap); got != StatePausedBattery {
		t.Fatalf("frío con batería baja: %q", got)
	}
}

// TestGameLoadOutranksThermal: while a game is loading the GPU the load
// reason is kept; after it, the thermal latch is reported over the load one.
func TestGameLoadOutranksThermal(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	load := SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 90, GPUDeviceUtilPct: 90, ThermalLevel: 2}
	pauseFor(t, ev, load)

	idle := load
	idle.GPUDeviceUtilPct = 0
	if got := ev.Evaluate(idle); got != StatePausedThermal {
		t.Fatalf("sin juego, aún caliente: %q", got)
	}
}

// TestFanlessPausesAtModeratePressure: a fanless Mac pauses at level 1 and
// stays paused until the level has been 0 for the whole cooldown; other
// profiles keep the level 2 threshold.
func TestFanlessPausesAtModeratePressure(t *testing.T) {
	cfg := DefaultConfig()
	e := NewEvaluator(cfg)
	fanless := SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 90, MacProfile: ProfileFanless}

	hot := fanless
	hot.ThermalLevel = ThermalPressureModerate
	if got := e.Evaluate(hot); got != StatePausedThermal {
		t.Fatalf("sin ventilador a nivel 1: %s", got)
	}
	for i := 1; i < cfg.CooldownTicksRequired; i++ {
		if got := e.Evaluate(fanless); got != StatePausedThermal {
			t.Fatalf("tick %d del enfriamiento: %s", i, got)
		}
	}
	if got := e.Evaluate(fanless); got != StateFolding {
		t.Fatalf("tras el enfriamiento: %s", got)
	}

	for _, p := range []MacThermalProfile{"", ProfileActiveLaptop, ProfileDesktop} {
		e := NewEvaluator(cfg)
		s := SystemSnapshot{OnAC: true, ThermalLevel: ThermalPressureModerate, MacProfile: p}
		if got := e.Evaluate(s); got != StateFolding {
			t.Fatalf("perfil %q a nivel 1: %s", p, got)
		}
		s.ThermalLevel = ThermalPressureSerious
		if got := e.Evaluate(s); got != StatePausedThermal {
			t.Fatalf("perfil %q a nivel 2: %s", p, got)
		}
	}
}
