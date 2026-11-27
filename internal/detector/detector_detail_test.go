// Intentionally has no build tag: Detail is platform-agnostic.
package detector

import (
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// TestDetailLoadCause: each kind of load is named, and the cause outlives the
// load itself for the whole cooldown.
func TestDetailLoadCause(t *testing.T) {
	pauseList := idleDesktopSnapshot()
	pauseList.UserPauseMatches = []string{"kcalc"}
	// A pause list match and a game in the same tick: the user's list wins.
	both := sessionSnap(60, testSession, testSession)
	both.UserPauseMatches = []string{"kcalc"}

	cases := []struct {
		name string
		snap SystemSnapshot
		want LoadCause
	}{
		{"lista de pausa", pauseList, CausePauseList},
		{"juego con sesión", sessionSnap(60, testSession, testSession), CauseGame},
		{"carga sin sesión", sessionSnap(60, ""), CauseGPULoad},
		{"carga global (macOS)", SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 90}, CauseGPULoad},
		{"contención de cómputo", computeSnap(7, 3, 1487, true), CauseCompute},
		{"lista y juego", both, CausePauseList},
	}
	for _, c := range cases {
		ev := NewEvaluator(DefaultConfig())
		pauseWith(t, ev, c.snap)
		if got := ev.Detail().LoadCause; got != c.want {
			t.Errorf("%s: causa = %q, se esperaba %q", c.name, got, c.want)
		}
		ev.Evaluate(idleDesktopSnapshot())
		if got := ev.Detail().LoadCause; got != c.want {
			t.Errorf("%s: causa en el cooldown = %q, se esperaba %q", c.name, got, c.want)
		}
	}
}

// TestDetailCooldownCountsDown: the ticks left fall by one per load-free tick,
// a load tick restarts them, and everything clears when folding resumes.
func TestDetailCooldownCountsDown(t *testing.T) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	game := SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 90}
	idle := idleDesktopSnapshot()

	if d := ev.Detail(); d != (Detail{}) {
		t.Fatalf("detalle inicial = %+v, se esperaba vacío", d)
	}
	ev.Evaluate(game)
	if d := ev.Detail(); d != (Detail{}) {
		t.Fatalf("detalle antes de pausar = %+v, se esperaba vacío", d)
	}
	pauseFor(t, ev, game)
	if d := ev.Detail(); d.LoadCooldownTicksLeft != 0 {
		t.Fatalf("con carga: quedan %d ticks, se esperaba 0", d.LoadCooldownTicksLeft)
	}

	for i := 1; i <= 10; i++ {
		ev.Evaluate(idle)
		if got, want := ev.Detail().LoadCooldownTicksLeft, cfg.CooldownTicksRequired-i; got != want {
			t.Fatalf("tick %d sin carga: quedan %d, se esperaba %d", i, got, want)
		}
	}
	ev.Evaluate(game)
	if got := ev.Detail().LoadCooldownTicksLeft; got != 0 {
		t.Fatalf("carga en el cooldown: quedan %d, se esperaba 0", got)
	}
	for i := 1; i < cfg.CooldownTicksRequired; i++ {
		ev.Evaluate(idle)
	}
	if got := ev.Detail().LoadCooldownTicksLeft; got != 1 {
		t.Fatalf("último tick del cooldown: quedan %d, se esperaba 1", got)
	}
	if got := ev.Evaluate(idle); got != StateFolding {
		t.Fatalf("fin del cooldown = %q, se esperaba %q", got, StateFolding)
	}
	if d := ev.Detail(); d != (Detail{}) {
		t.Fatalf("detalle tras reanudar = %+v, se esperaba vacío", d)
	}
}

// TestDetailNoCooldownWhileSessionHolds: a game in its menu keeps the pause
// without starting the cooldown, so there is no countdown to show.
func TestDetailNoCooldownWhileSessionHolds(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	pauseWith(t, ev, sessionSnap(60, testSession, testSession))
	ev.Evaluate(sessionSnap(5, testSession, testSession))
	if d := ev.Detail(); d.LoadCause != CauseGame || d.LoadCooldownTicksLeft != 0 {
		t.Fatalf("juego en el menú: detalle = %+v, se esperaba causa game sin cuenta", d)
	}
	ev.Evaluate(SystemSnapshot{OnAC: true, GPUProcesses: []gpu.ProcessInfo{}})
	if got, want := ev.Detail().LoadCooldownTicksLeft, ev.cfg.CooldownTicksRequired-1; got != want {
		t.Fatalf("juego cerrado: quedan %d, se esperaba %d", got, want)
	}
}

// TestDetailThermalCooldown counts the thermal latch down the same way.
func TestDetailThermalCooldown(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	hot := SystemSnapshot{OnAC: true, ThermalLevel: ThermalPressureSerious}
	cool := SystemSnapshot{OnAC: true}

	ev.Evaluate(hot)
	if got := ev.Detail().ThermalCooldownTicksLeft; got != 0 {
		t.Fatalf("en caliente: quedan %d, se esperaba 0", got)
	}
	ev.Evaluate(cool)
	ev.Evaluate(cool)
	if got, want := ev.Detail().ThermalCooldownTicksLeft, ev.cfg.CooldownTicksRequired-2; got != want {
		t.Fatalf("enfriando: quedan %d, se esperaba %d", got, want)
	}
	if d := ev.Detail(); d.LoadCause != "" {
		t.Fatalf("pausa térmica con causa de carga %q", d.LoadCause)
	}
}
