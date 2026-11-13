package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// TestPauseOnlyModeNeverTouchesPowerLimit covers macOS (no power-limit
// support): the daemon only pauses and resumes FAH.
func TestPauseOnlyModeNeverTouchesPowerLimit(t *testing.T) {
	h := newHarness(t, false)
	if err := h.d.start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	h.ticks(1, snapIdle)
	h.ticks(sustainTicks, snapLoad)
	h.ticks(cooldownTicks, snapIdle)

	for _, c := range h.log.all() {
		if strings.HasPrefix(c, "gpu.Set") || c == "gpu.Restore" {
			t.Fatalf("llamada de potencia inesperada %q en modo solo-pausa: %v", c, h.log.all())
		}
	}
	if got := h.log.count("fah.Pause"); got != 1 {
		t.Fatalf("Pause = %d, se esperaba 1", got)
	}
	if got := h.log.count("fah.Resume"); got != 1 {
		t.Fatalf("Resume = %d, se esperaba 1", got)
	}
}

// TestStartupRestoresDefaultLimit verifies that startup normalizes the power
// limit, so a cap left by a crashed instance never survives a restart.
func TestStartupRestoresDefaultLimit(t *testing.T) {
	h := newHarness(t, true)
	if err := h.d.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.log.count("gpu.Restore") != 1 {
		t.Fatalf("start no restauró el límite: %v", h.log.all())
	}
}

// TestStartupRestoreFailureRestoresOnFirstPause verifies that an unknown
// startup limit is treated as capped: the first pause restores it.
func TestStartupRestoreFailureRestoresOnFirstPause(t *testing.T) {
	h := newHarness(t, true)
	h.gpu.set(func(g *fakeGPU) { g.restoreErr = errors.New("nvml ocupado") })
	h.start(t)
	h.gpu.set(func(g *fakeGPU) { g.restoreErr = nil })

	h.ticks(1, snapLowBattery)
	if h.log.count("gpu.Restore") != 1 {
		t.Fatalf("la primera pausa no restauró el límite: %v", h.log.all())
	}
}

// TestEcoCapAppliedOnceWhileFolding verifies the 123 W cap is applied on the
// first folding tick and not reapplied every tick.
func TestEcoCapAppliedOnceWhileFolding(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)

	h.ticks(5, snapIdle)
	if got := h.log.count("gpu.Set(123)"); got != 1 {
		t.Fatalf("Set(123) = %d, se esperaba 1: %v", got, h.log.all())
	}
}

// TestEcoCapAppliedInFoldingEco verifies that StateFoldingEco also caps power
// (finding F1).
func TestEcoCapAppliedInFoldingEco(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)

	h.ticks(1, snapBatteryOK)
	if h.d.mode != ModeFoldingEco {
		t.Fatalf("modo = %s, se esperaba %s", h.d.mode, ModeFoldingEco)
	}
	if h.log.count("gpu.Set(123)") != 1 {
		t.Fatalf("StateFoldingEco no aplicó el tope: %v", h.log.all())
	}
}

// TestEcoWattsClampedToConstraints verifies the configured eco limit is
// clamped to the device constraints.
func TestEcoWattsClampedToConstraints(t *testing.T) {
	h := newHarness(t, true)
	h.gpu.set(func(g *fakeGPU) { g.tel.MinPowerLimitW = 130 })
	h.start(t)

	h.ticks(1, snapIdle)
	if h.log.count("gpu.Set(130)") != 1 {
		t.Fatalf("se esperaba Set(130) por el mínimo del dispositivo: %v", h.log.all())
	}
}

// TestLoadRestoresPowerBeforePausing verifies that the pause decision restores
// the default limit and pauses FAH in the same tick, power first (F7).
func TestLoadRestoresPowerBeforePausing(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)

	h.ticks(sustainTicks-1, snapLoad)
	if h.log.count("gpu.Restore") != 0 || h.log.count("fah.Pause") != 0 {
		t.Fatalf("acción antes de la carga sostenida: %v", h.log.all())
	}

	h.ticks(1, snapLoad)
	if h.d.mode != ModePausedLoad {
		t.Fatalf("modo = %s, se esperaba %s", h.d.mode, ModePausedLoad)
	}
	restore, pause := h.log.index("gpu.Restore"), h.log.index("fah.Pause")
	if restore < 0 || pause < 0 || restore > pause {
		t.Fatalf("se esperaba Restore antes de Pause en el mismo tick: %v", h.log.all())
	}
	if !h.markerPresent(t) {
		t.Fatal("falta el marcador tras una pausa confirmada")
	}
}

// TestCooldownCapsPowerBeforeResuming verifies that folding resumes only
// after the full cooldown, and that the cap is applied before Resume.
func TestCooldownCapsPowerBeforeResuming(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	h.ticks(sustainTicks, snapLoad)
	h.log.reset()

	h.ticks(cooldownTicks-1, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("Resume antes de terminar el cooldown: %v", h.log.all())
	}

	h.ticks(1, snapIdle)
	set, resume := h.log.index("gpu.Set(123)"), h.log.index("fah.Resume")
	if set < 0 || resume < 0 || set > resume {
		t.Fatalf("se esperaba Set(123) antes de Resume: %v", h.log.all())
	}
	if h.markerPresent(t) {
		t.Fatal("el marcador sigue tras un Resume confirmado")
	}
}

// TestResumeWaitsForPowerCap verifies that a failed SetPowerLimit keeps
// folding paused until the cap succeeds.
func TestResumeWaitsForPowerCap(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapLowBattery)
	h.gpu.set(func(g *fakeGPU) { g.setErr = errors.New("nvml: timeout") })
	h.log.reset()

	h.ticks(3, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("Resume sin tope aplicado: %v", h.log.all())
	}
	if h.log.count("gpu.Set(123)") != 3 {
		t.Fatalf("el tope no se reintentó en cada tick: %v", h.log.all())
	}

	h.gpu.set(func(g *fakeGPU) { g.setErr = nil })
	h.ticks(1, snapIdle)
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("Resume no ocurrió tras aplicar el tope: %v", h.log.all())
	}
}

// TestNoPermissionFallsBackToPauseOnly verifies that NVML_ERROR_NO_PERMISSION
// disables power control once and lets folding resume.
func TestNoPermissionFallsBackToPauseOnly(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapLowBattery)
	h.gpu.set(func(g *fakeGPU) { g.setErr = fmt.Errorf("SetPowerManagementLimit failed: %w", gpu.ErrNoPermission) })
	h.log.reset()

	h.ticks(1, snapIdle)
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("Resume no ocurrió en modo solo-pausa: %v", h.log.all())
	}
	h.ticks(5, snapIdle)
	if got := h.log.count("gpu.Set(123)"); got != 1 {
		t.Fatalf("Set(123) = %d tras NO_PERMISSION, se esperaba 1", got)
	}
}

// TestNoPermissionAtStartupFallsBackToPauseOnly verifies that a daemon
// started without the privileges to change the power limit never tries to cap
// or restore it, and still drives FAH.
func TestNoPermissionAtStartupFallsBackToPauseOnly(t *testing.T) {
	h := newHarness(t, true)
	h.gpu.set(func(g *fakeGPU) {
		g.restoreErr = fmt.Errorf("SetPowerManagementLimit failed: %w", gpu.ErrNoPermission)
	})
	h.start(t)

	h.ticks(1, snapIdle)
	h.ticks(1, snapLowBattery)
	h.ticks(1, snapIdle)
	for _, c := range h.log.all() {
		if strings.HasPrefix(c, "gpu.Set") || c == "gpu.Restore" {
			t.Fatalf("llamada de potencia %q sin permisos: %v", c, h.log.all())
		}
	}
	if h.log.count("fah.Pause") != 1 || h.log.count("fah.Resume") != 1 {
		t.Fatalf("FAH no se controló en modo solo-pausa: %v", h.log.all())
	}
	if err := h.d.failSafe(false); err != nil {
		t.Fatalf("failSafe sin permisos devolvió error: %v", err)
	}
	if h.log.count("gpu.Restore") != 0 {
		t.Fatal("failSafe intentó restaurar sin permisos")
	}
}

// TestBatteryPauseAndResumeWithoutCooldown verifies the battery guard.
func TestBatteryPauseAndResumeWithoutCooldown(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)

	h.ticks(1, snapLowBattery)
	if h.d.mode != ModePausedBattery || h.log.count("fah.Pause") != 1 {
		t.Fatalf("modo = %s, llamadas %v; se esperaba pausa por batería", h.d.mode, h.log.all())
	}
	h.ticks(1, snapIdle)
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("no se reanudó al volver a la corriente: %v", h.log.all())
	}
}

// TestUserPauseIsNeverResumed covers CH-10.
func TestUserPauseIsNeverResumed(t *testing.T) {
	h := newHarness(t, true)
	h.fah.set(func(f *fakeFAH) { f.status = fah.Status{Paused: true, State: fah.StatePaused} })
	h.start(t)

	h.ticks(1, snapIdle)
	h.ticks(sustainTicks, snapLoad)
	if h.log.count("fah.Status") != 1 || h.log.count("fah.Pause") != 0 {
		t.Fatalf("se esperaba consultar el estado y no pausar: %v", h.log.all())
	}
	h.ticks(cooldownTicks+5, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("se reanudó una pausa manual: %v", h.log.all())
	}
	if h.markerPresent(t) {
		t.Fatal("se escribió el marcador para una pausa del usuario")
	}
}

// TestPauseSentEvenWhenFAHLooksIdle covers CH2-H1.
func TestPauseSentEvenWhenFAHLooksIdle(t *testing.T) {
	cases := []struct {
		name      string
		status    fah.Status
		statusErr error
	}{
		{name: "idle", status: fah.Status{State: fah.StateIdle}},
		{name: "unknown", status: fah.Status{State: fah.StateUnknown}},
		{name: "status error", statusErr: errors.New("connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			h.fah.set(func(f *fakeFAH) { f.status, f.statusErr = tc.status, tc.statusErr })
			h.start(t)

			h.ticks(sustainTicks, snapLoad)
			if h.log.count("fah.Pause") != 1 {
				t.Fatalf("no se envió Pause: %v", h.log.all())
			}
		})
	}
}

// TestCollectErrorKeepsCooldown verifies decision 5: transient errors do not
// reach the evaluator, do not touch FAH or the GPU, and do not reset the
// cooldown.
func TestCollectErrorKeepsCooldown(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	h.ticks(sustainTicks, snapLoad)
	h.ticks(cooldownTicks/2, snapIdle)
	h.log.reset()
	evals := h.eval.count()

	h.failTicks(10, errors.New("nvml: gpu is lost"))
	if h.eval.count() != evals {
		t.Fatal("Evaluate se llamó con errores de Collect")
	}
	if len(h.log.all()) != 0 {
		t.Fatalf("acciones durante errores de Collect: %v", h.log.all())
	}
	if h.d.mode != ModePausedLoad {
		t.Fatalf("modo = %s, se esperaba mantener %s", h.d.mode, ModePausedLoad)
	}

	h.ticks(cooldownTicks-cooldownTicks/2-1, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatal("Resume antes de completar el cooldown")
	}
	h.ticks(1, snapIdle)
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("el cooldown se reinició por errores de Collect: %v", h.log.all())
	}
}

// TestCollectErrorKeepsSustainStreak verifies an error tick does not reset
// the high-load streak.
func TestCollectErrorKeepsSustainStreak(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)

	h.ticks(sustainTicks-1, snapLoad)
	h.failTicks(1, errors.New("ioreg timeout"))
	h.ticks(1, snapLoad)
	if h.d.mode != ModePausedLoad {
		t.Fatalf("modo = %s, se esperaba %s", h.d.mode, ModePausedLoad)
	}
}

// TestCollectErrorsAreRateLimited verifies a long outage does not log every
// tick.
func TestCollectErrorsAreRateLimited(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)

	h.failTicks(collectErrLogEvery*2, errors.New("ioreg timeout"))
	if got := countLines(h.logs.String(), "collect failed"); got != 3 {
		t.Fatalf("avisos de Collect = %d, se esperaban 3", got)
	}
}

// TestDegradedLatches covers M2-BLOCK.
func TestDegradedLatches(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	h.log.reset()

	h.failTicks(1, detector.ErrPerProcessUtilUnsupported)
	if h.d.mode != ModeDegraded {
		t.Fatalf("modo = %s, se esperaba %s", h.d.mode, ModeDegraded)
	}
	if h.log.count("gpu.Restore") != 1 {
		t.Fatalf("Degraded no restauró el límite: %v", h.log.all())
	}

	h.failTicks(3, detector.ErrPerProcessUtilUnsupported)
	h.ticks(3, snapIdle)
	h.ticks(3, snapLoad)
	for _, c := range h.log.all() {
		if strings.HasPrefix(c, "gpu.Set") || c == "fah.Pause" || c == "fah.Resume" {
			t.Fatalf("acción %q en Degraded: %v", c, h.log.all())
		}
	}
	if got := countLines(h.logs.String(), "per-process GPU utilization unsupported"); got != 1 {
		t.Fatalf("avisos de Degraded = %d, se esperaba 1", got)
	}
}

// TestDegradedCompletesPendingAction verifies a daemon-owned pending action is
// still completed in Degraded, so a pause never leaks half-done.
func TestDegradedCompletesPendingAction(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)
	h.fah.set(func(f *fakeFAH) { f.pauseErr = errors.New("fah reiniciando") })
	h.ticks(1, snapLowBattery)

	h.fah.set(func(f *fakeFAH) { f.pauseErr = nil })
	h.failTicks(1, detector.ErrPerProcessUtilUnsupported)
	if !h.d.pausedByUs || !h.markerPresent(t) {
		t.Fatal("la pausa pendiente no se completó en Degraded")
	}
}

// TestPauseRetriedUntilConfirmed verifies ownership is only taken after FAH
// confirms the pause.
func TestPauseRetriedUntilConfirmed(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)
	h.fah.set(func(f *fakeFAH) { f.pauseErr = errors.New("connection refused") })

	h.ticks(sustainTicks, snapLoad)
	if h.d.pausedByUs || h.markerPresent(t) {
		t.Fatal("se tomó la pausa como propia sin confirmación")
	}
	h.ticks(1, snapLoad)
	if got := h.log.count("fah.Pause"); got != 2 {
		t.Fatalf("Pause = %d, se esperaba reintento (2)", got)
	}

	h.fah.set(func(f *fakeFAH) { f.pauseErr = nil })
	h.ticks(1, snapLoad)
	if !h.d.pausedByUs || !h.markerPresent(t) {
		t.Fatal("la pausa confirmada no quedó registrada")
	}
}

// TestPendingPauseDroppedWhenDecisionFlips verifies that a pause that never
// landed is dropped, not resumed, when the decision goes back to folding.
func TestPendingPauseDroppedWhenDecisionFlips(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)
	h.fah.set(func(f *fakeFAH) { f.pauseErr = errors.New("connection refused") })

	h.ticks(1, snapLowBattery)
	h.ticks(1, snapIdle)
	h.fah.set(func(f *fakeFAH) { f.pauseErr = nil })
	h.log.reset()

	h.ticks(5, snapIdle)
	if len(h.log.all()) != 0 {
		t.Fatalf("acciones tras descartar la pausa pendiente: %v", h.log.all())
	}
}

// TestMarkerRestoresOwnershipAfterRestart verifies the crash-restart case of
// CH-10: a marker left by a previous instance lets the daemon resume its own
// pause.
func TestMarkerRestoresOwnershipAfterRestart(t *testing.T) {
	h := newHarness(t, true)
	if err := (marker{dir: h.dir}).set(); err != nil {
		t.Fatalf("marker.set: %v", err)
	}
	h.fah.set(func(f *fakeFAH) { f.status = fah.Status{Paused: true, State: fah.StatePaused} })
	h.start(t)
	if !h.d.pausedByUs {
		t.Fatal("el marcador no restauró pausedByUs")
	}

	hold := DefaultConfig().StartupHoldTicks
	h.ticks(hold, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("Resume antes de terminar la espera de arranque: %v", h.log.all())
	}
	h.ticks(1, snapIdle)
	set, resume := h.log.index("gpu.Set(123)"), h.log.index("fah.Resume")
	if set < 0 || resume < 0 || set > resume {
		t.Fatalf("se esperaba Set(123) y luego Resume: %v", h.log.all())
	}
	if h.markerPresent(t) {
		t.Fatal("el marcador sigue tras reanudar")
	}
}

// TestMarkerKeptWhenRestartFindsLoad verifies that a restarted daemon that
// sees load keeps owning its old pause without pausing again.
func TestMarkerKeptWhenRestartFindsLoad(t *testing.T) {
	h := newHarness(t, false)
	if err := (marker{dir: h.dir}).set(); err != nil {
		t.Fatalf("marker.set: %v", err)
	}
	h.fah.set(func(f *fakeFAH) { f.status = fah.Status{Paused: true, State: fah.StatePaused} })
	h.start(t)

	h.ticks(sustainTicks, snapLoad)
	if h.log.count("fah.Pause") != 0 || !h.d.pausedByUs {
		t.Fatalf("se esperaba conservar la pausa propia: %v", h.log.all())
	}
	h.ticks(cooldownTicks, snapIdle)
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("no se reanudó la pausa propia tras el cooldown: %v", h.log.all())
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("tiempo agotado esperando: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func runHarness(t *testing.T, h *harness) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	h.d.cfg.Tick = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- h.d.Run(ctx) }()
	return cancel, ch
}

// TestRunRestoresOnCancel covers the SIGTERM path: the default limit is
// restored, FAH is closed but not resumed, and the marker is kept.
func TestRunRestoresOnCancel(t *testing.T) {
	h := newHarness(t, true)
	h.col.set(snapLoad, nil)
	cancel, done := runHarness(t, h)
	waitFor(t, "pausa", func() bool { return h.log.count("fah.Pause") == 1 })

	before := h.log.count("gpu.Restore")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run devolvió error: %v", err)
	}
	if h.log.count("gpu.Restore") != before+1 {
		t.Fatalf("no se restauró el límite al salir: %v", h.log.all())
	}
	if h.log.count("fah.Resume") != 0 {
		t.Fatal("se reanudó FAH al apagar")
	}
	if h.log.count("fah.Close") != 1 || h.log.count("gpu.Shutdown") != 1 {
		t.Fatalf("faltan Close/Shutdown: %v", h.log.all())
	}
	if !h.markerPresent(t) {
		t.Fatal("el marcador debe conservarse al apagar")
	}
}

// TestRunRecoversPanic verifies a panic in the loop still restores power.
func TestRunRecoversPanic(t *testing.T) {
	h := newHarness(t, true)
	h.col.doPanic = true
	cancel, done := runHarness(t, h)
	defer cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run no terminó tras el panic")
	}
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err = %v, se esperaba un error de panic", err)
	}
	if h.log.count("gpu.Restore") != 2 { // startup + fail-safe
		t.Fatalf("no se restauró el límite tras el panic: %v", h.log.all())
	}
	if h.log.count("gpu.Shutdown") != 1 {
		t.Fatal("falta gpu.Shutdown tras el panic")
	}
}

// TestRunBoundsHungRestore covers M4: a restore that never returns cannot
// block shutdown forever.
func TestRunBoundsHungRestore(t *testing.T) {
	h := newHarness(t, true)
	h.d.cfg.RestoreTimeout = 50 * time.Millisecond
	cancel, done := runHarness(t, h)
	waitFor(t, "primer tick", func() bool { return h.col.callCount() > 0 })

	block := make(chan struct{})
	defer close(block)
	h.gpu.set(func(g *fakeGPU) { g.restoreBlock = block })
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("se esperaba error por restauración colgada")
		}
	case <-time.After(time.Second):
		t.Fatal("el apagado quedó bloqueado por la restauración")
	}
	if h.log.count("gpu.Shutdown") != 0 {
		t.Fatal("Shutdown no debe llamarse con una llamada NVML colgada")
	}
}

// TestRunFailsWhenInitFails verifies a GPU init error is fatal and nothing
// else runs.
func TestRunFailsWhenInitFails(t *testing.T) {
	h := newHarness(t, true)
	h.gpu.set(func(g *fakeGPU) { g.initErr = errors.New("nvml: driver not loaded") })

	if err := h.d.Run(context.Background()); err == nil {
		t.Fatal("se esperaba error de Init")
	}
	if h.col.callCount() != 0 || h.log.count("gpu.Restore") != 0 {
		t.Fatalf("acciones tras fallar Init: %v", h.log.all())
	}
}

// TestNewValidatesDeps verifies New rejects missing dependencies.
func TestNewValidatesDeps(t *testing.T) {
	log := &callLog{}
	ok := Deps{GPU: newFakeGPU(log, false), FAH: newFakeFAH(log), Collector: &scriptedCollector{}, Evaluator: detector.NewEvaluator(detector.DefaultConfig())}
	cfg := DefaultConfig()
	cfg.RuntimeDir = t.TempDir()

	if _, err := New(cfg, ok); err != nil {
		t.Fatalf("New con dependencias válidas: %v", err)
	}
	noDir := cfg
	noDir.RuntimeDir = ""
	if _, err := New(noDir, ok); err == nil {
		t.Fatal("se esperaba error sin RuntimeDir")
	}
	for name, mutate := range map[string]func(*Deps){
		"gpu":       func(d *Deps) { d.GPU = nil },
		"fah":       func(d *Deps) { d.FAH = nil },
		"collector": func(d *Deps) { d.Collector = nil },
		"evaluator": func(d *Deps) { d.Evaluator = nil },
	} {
		deps := ok
		mutate(&deps)
		if _, err := New(cfg, deps); err == nil {
			t.Fatalf("se esperaba error sin %s", name)
		}
	}
}
