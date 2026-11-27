package daemon

import (
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
)

// TestStatusLoadCauseAndCooldown: the status names the cause of a load pause
// for its whole duration, counts the cooldown down in seconds and clears both
// once folding resumes.
func TestStatusLoadCauseAndCooldown(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	list := detector.SystemSnapshot{OnAC: true, UserPauseMatches: []string{"kcalc"}}

	h.ticks(sustainTicks, list)
	st := h.d.publish()
	if st.Mode != ModePausedLoad || st.LoadCause != "pause_list" || st.CooldownS != 0 {
		t.Fatalf("con kcalc abierto: modo %q, causa %q, cooldown %d s", st.Mode, st.LoadCause, st.CooldownS)
	}

	h.ticks(1, snapIdle)
	st = h.d.publish()
	want := (cooldownTicks - 1) * int(h.d.cfg.Tick.Seconds())
	if st.Mode != ModePausedLoad || st.LoadCause != "pause_list" || st.CooldownS != want {
		t.Fatalf("kcalc cerrado: modo %q, causa %q, cooldown %d s (se esperaba pause_list, %d s)",
			st.Mode, st.LoadCause, st.CooldownS, want)
	}

	h.ticks(cooldownTicks-1, snapIdle)
	st = h.d.publish()
	if st.Mode != ModeFolding || st.LoadCause != "" || st.CooldownS != 0 {
		t.Fatalf("tras el cooldown: modo %q, causa %q, cooldown %d s", st.Mode, st.LoadCause, st.CooldownS)
	}
}

// TestStatusThermalCooldown: a thermal pause has no load cause but counts its
// own cooldown down.
func TestStatusThermalCooldown(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapHot)
	h.ticks(3, snapBatteryOK)
	st := h.d.publish()
	want := (cooldownTicks - 3) * int(h.d.cfg.Tick.Seconds())
	if st.Mode != ModePausedThermal || st.LoadCause != "" || st.CooldownS != want {
		t.Fatalf("enfriando: modo %q, causa %q, cooldown %d s (se esperaba %d s)", st.Mode, st.LoadCause, st.CooldownS, want)
	}
}

// TestStatusManualPauseHasNoCause: a manual pause made while folding is the
// user's, with no load cause or countdown.
func TestStatusManualPauseHasNoCause(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	st, err := h.forcePause()
	if err != nil || st.Mode != ModePausedManual || st.LoadCause != "" || st.CooldownS != 0 {
		t.Fatalf("pausa manual: err %v, modo %q, causa %q, cooldown %d s", err, st.Mode, st.LoadCause, st.CooldownS)
	}
}
