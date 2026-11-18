package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
)

// run executes a command on the test goroutine, as the loop would between
// ticks, and returns the status it publishes.
func (h *harness) run(c *command) (Status, error) {
	err := h.d.exec(context.Background(), c)
	return h.d.publish(), err
}

func (h *harness) forcePause() (Status, error)  { return h.run(&command{kind: cmdForcePause}) }
func (h *harness) forceResume() (Status, error) { return h.run(&command{kind: cmdForceResume}) }
func (h *harness) setEcoWatts(w int) (Status, error) {
	return h.run(&command{kind: cmdSetEcoWatts, watts: w})
}

// userPausesFAH simulates the user pausing in the FAH client.
func (h *harness) userPausesFAH() {
	h.fah.set(func(f *fakeFAH) { f.status = fah.Status{Paused: true, State: fah.StatePaused} })
}

func (h *harness) noPowerCalls(t *testing.T) {
	t.Helper()
	for _, c := range h.log.all() {
		if strings.HasPrefix(c, "gpu.Set") || c == "gpu.Restore" {
			t.Fatalf("llamada de potencia inesperada %q: %v", c, h.log.all())
		}
	}
}

// settableCollector is a scriptedCollector that accepts a pause list.
type settableCollector struct {
	*scriptedCollector
	mu       sync.Mutex
	patterns []string
	sets     int
}

func (c *settableCollector) SetPauseProcesses(p []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.patterns = append([]string(nil), p...)
	c.sets++
}

func (c *settableCollector) get() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patterns, c.sets
}

// TestForcePauseRestoresPausesAndHolds: force_pause restores the default
// limit, pauses FAH as the daemon's own pause, and the evaluator's "fold"
// never resumes it.
func TestForcePauseRestoresPausesAndHolds(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(2, snapIdle)
	if !h.d.capApplied {
		t.Fatal("el tope eco no estaba aplicado antes de la pausa manual")
	}
	h.log.reset()

	st, err := h.forcePause()
	if err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	restore, pause := h.log.index("gpu.Restore"), h.log.index("fah.Pause")
	if restore < 0 || pause < 0 || restore > pause {
		t.Fatalf("se esperaba Restore antes de Pause: %v", h.log.all())
	}
	if !h.d.pausedByUs || !h.markerPresent(t) {
		t.Fatalf("la pausa manual no quedó como pausa del daemon (pausedByUs=%v)", h.d.pausedByUs)
	}
	if st.ManualOverride != OverridePause || st.Mode != ModePausedManual || !st.PausedByUs || st.CapApplied {
		t.Fatalf("status = %+v", st)
	}
	if !st.FAH.Paused || !st.FAH.Reachable || st.FAH.ObservedAt.IsZero() {
		t.Fatalf("estado FAH = %+v", st.FAH)
	}

	h.log.reset()
	h.ticks(cooldownTicks+10, snapIdle)
	if h.log.count("fah.Resume") != 0 || h.log.count("gpu.Set(123)") != 0 {
		t.Fatalf("el evaluador reanudó o limitó durante la pausa manual: %v", h.log.all())
	}
	if h.d.mode != ModePausedManual || h.d.lastDecision != detector.StateFolding {
		t.Fatalf("modo = %s, decisión = %s", h.d.mode, h.d.lastDecision)
	}

	// A second force_pause changes nothing.
	if _, err := h.forcePause(); err != nil {
		t.Fatalf("segundo force_pause: %v", err)
	}
	if h.log.count("fah.Pause") != 0 {
		t.Fatalf("Pause repetido: %v", h.log.all())
	}
}

// TestForcePauseLeavesUserPauseAlone: FAH already paused by the user (CH-10)
// keeps its ownership.
func TestForcePauseLeavesUserPauseAlone(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	h.userPausesFAH()
	h.log.reset()

	st, err := h.forcePause()
	if err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	if h.log.count("fah.Pause") != 0 || h.d.pausedByUs || h.markerPresent(t) {
		t.Fatalf("la pausa del usuario cambió de dueño: pausedByUs=%v, %v", h.d.pausedByUs, h.log.all())
	}
	if st.PausedByUs || st.ManualOverride != OverridePause || h.log.count("gpu.Restore") != 1 {
		t.Fatalf("status = %+v, llamadas = %v", st, h.log.all())
	}
}

// TestForceResumeCapsBeforeResume pins I2 for the manual path and the
// documented exception to I1: an explicit resume also resumes a user pause.
func TestForceResumeCapsBeforeResume(t *testing.T) {
	for _, userPause := range []bool{false, true} {
		t.Run(fmt.Sprintf("pausa del usuario=%v", userPause), func(t *testing.T) {
			h := newHarness(t, true)
			h.start(t)
			h.ticks(1, snapIdle)
			if userPause {
				h.userPausesFAH()
			}
			if _, err := h.forcePause(); err != nil {
				t.Fatalf("force_pause: %v", err)
			}
			h.ticks(3, snapIdle)
			h.log.reset()

			st, err := h.forceResume()
			if err != nil {
				t.Fatalf("force_resume: %v", err)
			}
			set, resume := h.log.index("gpu.Set(123)"), h.log.index("fah.Resume")
			if set < 0 || resume < 0 || set > resume {
				t.Fatalf("se esperaba Set(123) antes de Resume: %v", h.log.all())
			}
			if h.d.pausedByUs || h.markerPresent(t) {
				t.Fatal("la propiedad de la pausa no se liberó")
			}
			if st.ManualOverride != OverrideNone || st.Mode != ModeFolding || !st.CapApplied || st.FAH.Paused {
				t.Fatalf("status = %+v", st)
			}

			h.log.reset()
			h.ticks(3, snapIdle)
			if got := h.log.all(); len(got) != 0 {
				t.Fatalf("acciones tras force_resume: %v", got)
			}
		})
	}
}

// TestForceResumeCapFailureDoesNotResume: no cap, no folding. The override is
// cleared, so the loop resumes the daemon's pause once the cap succeeds.
func TestForceResumeCapFailureDoesNotResume(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	if _, err := h.forcePause(); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	h.gpu.set(func(g *fakeGPU) { g.setErr = errors.New("nvml ocupado") })
	h.log.reset()

	st, err := h.forceResume()
	if !errors.Is(err, ErrCapFailed) {
		t.Fatalf("err = %v, se esperaba ErrCapFailed", err)
	}
	if h.log.count("fah.Resume") != 0 || h.log.count("fah.Status") != 0 {
		t.Fatalf("FAH tocado sin tope: %v", h.log.all())
	}
	if st.ManualOverride != OverrideNone || !st.PausedByUs || st.CapApplied {
		t.Fatalf("status = %+v", st)
	}

	h.ticks(2, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("reanudado sin tope: %v", h.log.all())
	}
	h.gpu.set(func(g *fakeGPU) { g.setErr = nil })
	h.log.reset()
	h.ticks(1, snapIdle)
	set, resume := h.log.index("gpu.Set(123)"), h.log.index("fah.Resume")
	if set < 0 || resume < 0 || set > resume {
		t.Fatalf("el loop no reanudó tras recuperar el tope: %v", h.log.all())
	}
}

// TestForceResumeRejectedWhilePaused: the evaluator's pause wins over an
// explicit resume.
func TestForceResumeRejectedWhilePaused(t *testing.T) {
	cases := []struct {
		name  string
		snap  detector.SystemSnapshot
		ticks int
		want  error
		mode  Mode
	}{
		{"juego activo", snapLoad, sustainTicks, ErrGameActive, ModePausedLoad},
		{"batería baja", snapLowBattery, 1, ErrOnBattery, ModePausedBattery},
		{"presión térmica", snapHot, 1, ErrThermal, ModePausedThermal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			h.start(t)
			h.ticks(1, snapIdle)
			h.ticks(tc.ticks, tc.snap)
			if _, err := h.forcePause(); err != nil {
				t.Fatalf("force_pause: %v", err)
			}
			h.log.reset()

			st, err := h.forceResume()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, se esperaba %v", err, tc.want)
			}
			if got := h.log.all(); len(got) != 0 {
				t.Fatalf("acciones con la pausa del evaluador activa: %v", got)
			}
			if st.Mode != tc.mode || st.ManualOverride != OverrideNone || !st.PausedByUs {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

// TestForcePauseOutlivesGame: a manual pause made during a game is still
// there after the game's cooldown, and force_resume then works.
func TestForcePauseOutlivesGame(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	h.ticks(sustainTicks, snapLoad)
	h.log.reset()

	if _, err := h.forcePause(); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	if h.log.count("fah.Pause") != 0 {
		t.Fatalf("Pause repetido sobre la pausa del daemon: %v", h.log.all())
	}
	h.ticks(cooldownTicks+5, snapIdle)
	if h.log.count("fah.Resume") != 0 || h.d.mode != ModePausedManual {
		t.Fatalf("la pausa manual no sobrevivió al juego: modo %s, %v", h.d.mode, h.log.all())
	}
	if _, err := h.forceResume(); err != nil {
		t.Fatalf("force_resume: %v", err)
	}
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("no se reanudó: %v", h.log.all())
	}
}

// TestForceResumeInDegraded: power control is off, so folding resumes
// without a cap and the status says so.
func TestForceResumeInDegraded(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	h.failTicks(1, detector.ErrPerProcessUtilUnsupported)
	if _, err := h.forcePause(); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	h.log.reset()

	st, err := h.forceResume()
	if err != nil {
		t.Fatalf("force_resume: %v", err)
	}
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("no se reanudó: %v", h.log.all())
	}
	h.noPowerCalls(t)
	if !st.Degraded || st.PowerEnabled || st.CapApplied || st.Mode != ModeDegraded {
		t.Fatalf("status = %+v", st)
	}
}

// TestForceResumeDuringStartupHold: the explicit request outranks the hold
// of an inherited pause, and the next ticks keep the cap.
func TestForceResumeDuringStartupHold(t *testing.T) {
	h := newHarness(t, true)
	if err := (marker{dir: h.dir}).set(originAuto); err != nil {
		t.Fatalf("marker.set: %v", err)
	}
	h.userPausesFAH() // FAH is paused, and the marker says it is ours
	h.start(t)
	h.ticks(2, snapIdle)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("reanudado durante la espera de arranque: %v", h.log.all())
	}
	h.log.reset()

	if _, err := h.forceResume(); err != nil {
		t.Fatalf("force_resume: %v", err)
	}
	h.ticks(3, snapIdle)
	set, resume := h.log.index("gpu.Set(123)"), h.log.index("fah.Resume")
	if set < 0 || resume < 0 || set > resume || h.log.count("gpu.Restore") != 0 {
		t.Fatalf("se esperaba Set(123), Resume y ningún Restore: %v", h.log.all())
	}
}

// TestForcePauseRetriesWhenFAHIsDown: the override holds and the pause lands
// when FAH comes back.
func TestForcePauseRetriesWhenFAHIsDown(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	down := errors.New("fah caído")
	h.fah.set(func(f *fakeFAH) { f.statusErr, f.pauseErr = down, down })

	st, err := h.forcePause()
	if !errors.Is(err, ErrFAHUnavailable) {
		t.Fatalf("err = %v, se esperaba ErrFAHUnavailable", err)
	}
	if st.ManualOverride != OverridePause || st.PausedByUs || st.FAH.Reachable || st.CapApplied {
		t.Fatalf("status = %+v", st)
	}
	h.ticks(2, snapIdle)
	if h.d.pausedByUs {
		t.Fatal("propiedad tomada sin confirmación de FAH")
	}

	h.fah.set(func(f *fakeFAH) { f.statusErr, f.pauseErr = nil, nil })
	h.ticks(1, snapIdle)
	if !h.d.pausedByUs || !h.markerPresent(t) {
		t.Fatalf("la pausa pendiente no se completó: %v", h.log.all())
	}
}

func TestSetEcoWatts(t *testing.T) {
	t.Run("fuera de rango", func(t *testing.T) {
		h := newHarness(t, true)
		h.start(t)
		h.ticks(1, snapIdle)
		h.log.reset()
		for _, w := range []int{99, 146, 0, -5, 1 << 40} {
			st, err := h.setEcoWatts(w)
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("%d W: err = %v, se esperaba ErrOutOfRange", w, err)
			}
			if st.EcoWatts != 123 {
				t.Fatalf("%d W: eco = %d, se esperaba 123", w, st.EcoWatts)
			}
		}
		h.noPowerCalls(t)
	})

	t.Run("con el tope aplicado", func(t *testing.T) {
		h := newHarness(t, true)
		h.start(t)
		h.ticks(1, snapIdle)
		h.log.reset()
		for _, w := range []int{100, 145, 110} {
			st, err := h.setEcoWatts(w)
			if err != nil || st.EcoWatts != w {
				t.Fatalf("%d W: err = %v, eco = %d", w, err, st.EcoWatts)
			}
			if h.log.count(fmt.Sprintf("gpu.Set(%d)", w)) != 1 {
				t.Fatalf("%d W no se reaplicó: %v", w, h.log.all())
			}
		}
		h.log.reset()
		if _, err := h.setEcoWatts(110); err != nil || len(h.log.all()) != 0 {
			t.Fatalf("mismo valor: err = %v, llamadas = %v", err, h.log.all())
		}

		h.gpu.set(func(g *fakeGPU) { g.setErr = errors.New("nvml ocupado") })
		st, err := h.setEcoWatts(130)
		if !errors.Is(err, ErrCapFailed) || st.EcoWatts != 110 || h.d.ecoWatts != 110 {
			t.Fatalf("fallo de NVML: err = %v, eco = %d", err, st.EcoWatts)
		}
	})

	t.Run("en pausa no toca la GPU", func(t *testing.T) {
		h := newHarness(t, true)
		h.start(t)
		h.ticks(1, snapIdle)
		h.ticks(sustainTicks, snapLoad)
		h.log.reset()
		if st, err := h.setEcoWatts(110); err != nil || st.EcoWatts != 110 {
			t.Fatalf("err = %v, eco = %d", err, st.EcoWatts)
		}
		h.noPowerCalls(t)

		h.ticks(cooldownTicks, snapIdle)
		if h.log.count("gpu.Set(110)") != 1 || h.log.count("gpu.Set(123)") != 0 {
			t.Fatalf("el nuevo tope no se usó al reanudar: %v", h.log.all())
		}
	})

	t.Run("con pausa manual no toca la GPU", func(t *testing.T) {
		h := newHarness(t, true)
		h.start(t)
		h.ticks(1, snapIdle)
		// A failed restore leaves capApplied set although no cap is wanted.
		h.gpu.set(func(g *fakeGPU) { g.restoreErr = errors.New("nvml ocupado") })
		_, _ = h.forcePause()
		if !h.d.capApplied {
			t.Fatal("se esperaba un restore pendiente")
		}
		h.log.reset()
		if _, err := h.setEcoWatts(110); err != nil {
			t.Fatalf("err = %v", err)
		}
		if h.log.count("gpu.Set(110)") != 0 {
			t.Fatalf("GPU limitada durante una pausa manual: %v", h.log.all())
		}
	})

	t.Run("sin control de potencia", func(t *testing.T) {
		h := newHarness(t, false)
		h.start(t)
		h.ticks(1, snapIdle)
		if _, err := h.setEcoWatts(120); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, se esperaba ErrUnsupported", err)
		}
		h.noPowerCalls(t)
	})
}

func TestValidatePausePatterns(t *testing.T) {
	got, err := ValidatePausePatterns([]string{" Blender ", "obs*", "blender", ""})
	if err != nil || strings.Join(got, "|") != "blender|obs*" {
		t.Fatalf("lista válida: %v, %v", got, err)
	}
	if got, err := ValidatePausePatterns(nil); err != nil || len(got) != 0 {
		t.Fatalf("lista vacía: %v, %v", got, err)
	}

	many := make([]string, maxPausePatterns+1)
	for i := range many {
		many[i] = fmt.Sprintf("app%d", i)
	}
	for name, list := range map[string][]string{
		"coma":            {"blender,obs"},
		"solo coma":       {","},
		"FahCore":         {"FahCore_*"},
		"fahclient":       {"blender", "fahclient"},
		"prefijo de fah":  {"fah*"},
		"comodín total":   {"*"},
		"ruta":            {"/usr/bin/blender"},
		"comodín interno": {"ble*nder"},
		"demasiado largo": {strings.Repeat("a", 65)},
		"65 patrones":     many,
		"byte nulo":       {"a\x00b"},
	} {
		if got, err := ValidatePausePatterns(list); !errors.Is(err, ErrInvalidPatterns) {
			t.Errorf("%s: %v, err = %v; se esperaba ErrInvalidPatterns", name, got, err)
		}
	}
	if _, err := ValidatePausePatterns(many[:maxPausePatterns]); err != nil {
		t.Fatalf("64 patrones: %v", err)
	}
}

func TestSetPauseProcesses(t *testing.T) {
	t.Run("collector sin lista", func(t *testing.T) {
		h := newHarness(t, true)
		h.start(t)
		st, err := h.run(&command{kind: cmdSetPauseProcesses, patterns: []string{"blender"}})
		if !errors.Is(err, ErrUnsupported) || len(st.PauseProcesses) != 0 || st.PauseListSupported {
			t.Fatalf("err = %v, status = %+v", err, st)
		}
	})

	t.Run("collector con lista", func(t *testing.T) {
		h := newHarness(t, true)
		col := &settableCollector{scriptedCollector: h.col}
		h.d.collector = col
		h.d.cfg.PauseProcesses = []string{"resolve"}
		h.start(t)
		if got, _ := col.get(); strings.Join(got, "|") != "resolve" {
			t.Fatalf("la lista inicial no llegó al collector: %v", got)
		}
		if st := h.d.Status(); strings.Join(st.PauseProcesses, "|") != "resolve" || !st.PauseListSupported {
			t.Fatalf("status inicial = %v", st.PauseProcesses)
		}

		st, err := h.run(&command{kind: cmdSetPauseProcesses, patterns: []string{"blender", "obs*"}})
		if err != nil || strings.Join(st.PauseProcesses, "|") != "blender|obs*" {
			t.Fatalf("err = %v, lista = %v", err, st.PauseProcesses)
		}
		if got, _ := col.get(); strings.Join(got, "|") != "blender|obs*" {
			t.Fatalf("collector = %v", got)
		}

		st, err = h.run(&command{kind: cmdSetPauseProcesses})
		if got, _ := col.get(); err != nil || len(got) != 0 || len(st.PauseProcesses) != 0 {
			t.Fatalf("borrar la lista: err = %v, collector = %v, status = %v", err, got, st.PauseProcesses)
		}
	})

	t.Run("la lista inválida no llega al loop", func(t *testing.T) {
		h := newHarness(t, true)
		col := &settableCollector{scriptedCollector: h.col}
		h.d.collector = col
		// The loop is not running: only validation can answer this fast.
		for _, list := range [][]string{{"a,b"}, {"FahCore_*"}} {
			if _, err := h.d.SetPauseProcesses(context.Background(), list); !errors.Is(err, ErrInvalidPatterns) {
				t.Fatalf("%v: err = %v, se esperaba ErrInvalidPatterns", list, err)
			}
		}
		if _, sets := col.get(); sets != 0 {
			t.Fatalf("el collector recibió %d listas", sets)
		}
	})
}

// TestStatusBeforeStartAndIsolation: Status works before Run and callers
// cannot corrupt the published snapshot.
func TestStatusBeforeStartAndIsolation(t *testing.T) {
	h := newHarness(t, true)
	if st := h.d.Status(); st.Mode != ModeStarting || st.ManualOverride != OverrideNone || st.GameSessions == nil {
		t.Fatalf("status antes de arrancar = %+v", st)
	}

	h.start(t)
	snap := snapIdle
	snap.LiveGameSessions = []string{"steam:570"}
	snap.UserPauseMatches = []string{"blender"}
	snap.HasBattery, snap.BatteryPct = true, 80
	h.ticks(1, snap)
	h.d.publish()

	st := h.d.Status()
	if st.PowerLimitW != 145 || st.MinPowerLimitW != 100 || st.MaxPowerLimitW != 145 || st.DefaultPowerLimitW != 145 ||
		st.EcoWatts != 123 || !st.PowerCapable || !st.PowerEnabled || st.GPU.Name != "Fake RTX 5060" {
		t.Fatalf("status = %+v", st)
	}
	if !st.Battery.Present || st.Battery.Pct != 80 || st.UpdatedAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	st.GameSessions[0], st.PauseMatches[0] = "corrupto", "corrupto"
	again := h.d.Status()
	if again.GameSessions[0] != "steam:570" || again.PauseMatches[0] != "blender" {
		t.Fatalf("un lector modificó el snapshot publicado: %+v", again)
	}
}

// runLoop starts Run with a fast tick and waits for the first tick.
func (h *harness) runLoop(t *testing.T) (cancel context.CancelFunc, done chan error) {
	t.Helper()
	h.d.cfg.Tick = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for h.d.Status().Mode == ModeStarting {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("el loop no publicó su primer tick")
		}
		time.Sleep(time.Millisecond)
	}
	return cancel, done
}

// TestCommandsAndStatusWhileLoopRuns hammers Status from 20 goroutines and
// sends commands from others while the loop ticks. Run it with -race.
func TestCommandsAndStatusWhileLoopRuns(t *testing.T) {
	h := newHarness(t, true)
	col := &settableCollector{scriptedCollector: h.col}
	h.d.collector = col
	cancel, done := h.runLoop(t)

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup
	for i := 0; i < 20; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				st := h.d.Status()
				if st.EcoWatts < 100 || st.EcoWatts > 145 || st.Mode == "" {
					t.Errorf("status incoherente: %+v", st)
					return
				}
				// Readers own their copy.
				st.PauseProcesses = append(st.PauseProcesses, "x")
			}
		}()
	}

	ctx := context.Background()
	errs := make(chan error, 64)
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			for j := 0; j < 25; j++ {
				var err error
				switch (i + j) % 4 {
				case 0:
					_, err = h.d.ForcePause(ctx)
				case 1:
					_, err = h.d.ForceResume(ctx)
				case 2:
					_, err = h.d.SetEcoWatts(ctx, 100+(i*7+j)%46)
				case 3:
					_, err = h.d.SetPauseProcesses(ctx, []string{fmt.Sprintf("app%d", j), "obs*"})
				}
				if err != nil {
					errs <- fmt.Errorf("goroutine %d, comando %d: %w", i, j, err)
				}
			}
		}(i)
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	st, err := h.d.ForcePause(ctx)
	if err != nil || st.ManualOverride != OverridePause || !st.FAH.Paused {
		t.Fatalf("force_pause final: %v, %+v", err, st)
	}
	if got := h.d.Status(); got.ManualOverride != OverridePause {
		t.Fatalf("Status no refleja el comando: %+v", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCommandWhileStopping: commands fail fast with ErrStopping once
// shutdown begins, and Status keeps working.
func TestCommandWhileStopping(t *testing.T) {
	h := newHarness(t, true)
	cancel, done := h.runLoop(t)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.log.reset()

	for name, call := range map[string]func() (Status, error){
		"force_pause":   func() (Status, error) { return h.d.ForcePause(context.Background()) },
		"force_resume":  func() (Status, error) { return h.d.ForceResume(context.Background()) },
		"set_eco_watts": func() (Status, error) { return h.d.SetEcoWatts(context.Background(), 120) },
	} {
		start := time.Now()
		st, err := call()
		if !errors.Is(err, ErrStopping) {
			t.Fatalf("%s: err = %v, se esperaba ErrStopping", name, err)
		}
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("%s bloqueó %v durante el apagado", name, d)
		}
		if st.Mode == "" {
			t.Fatalf("%s: status vacío", name)
		}
	}
	if got := h.log.all(); len(got) != 0 {
		t.Fatalf("acciones tras el apagado: %v", got)
	}
}

// TestCommandDuringShutdownDoesNotBlock: a command waiting on a loop that is
// stuck in a slow tick returns as soon as shutdown begins.
func TestCommandDuringShutdownDoesNotBlock(t *testing.T) {
	h := newHarness(t, true)
	h.d.cfg.ShutdownGrace = 2 * time.Second
	cancel, done := h.runLoop(t)

	// Make every tick slow: a pause decision with a slow FAH.
	h.fah.set(func(f *fakeFAH) { f.delay = 300 * time.Millisecond })
	h.col.set(snapLowBattery, nil)
	time.Sleep(20 * time.Millisecond)

	res := make(chan error, 1)
	go func() {
		_, err := h.d.SetEcoWatts(context.Background(), 120)
		res <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-res:
		// The loop may have taken the command just before stopping.
		if err != nil && !errors.Is(err, ErrStopping) {
			t.Fatalf("err = %v, se esperaba nil o ErrStopping", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("el comando quedó bloqueado durante el apagado")
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCommandTimesOutWhenLoopIsBusy: a loop stuck in a tick answers ErrBusy
// after 3 s, and the abandoned command never runs afterwards.
func TestCommandTimesOutWhenLoopIsBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("espera el timeout real de 3 s")
	}
	h := newHarness(t, true)
	h.d.cfg.ActionTimeout = 10 * time.Second
	cancel, done := h.runLoop(t)
	defer func() {
		cancel()
		<-done
	}()

	h.fah.set(func(f *fakeFAH) { f.delay = 4 * time.Second })
	h.col.set(snapLowBattery, nil)
	time.Sleep(50 * time.Millisecond) // the loop is now inside a slow FAH call

	start := time.Now()
	_, err := h.d.SetEcoWatts(context.Background(), 110)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, se esperaba ErrBusy", err)
	}
	if d := time.Since(start); d < 2500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("ErrBusy tras %v, se esperaban ~3 s", d)
	}

	// A caller's own deadline wins when it is shorter.
	ctx, cancelCtx := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelCtx()
	if _, err := h.d.SetEcoWatts(ctx, 111); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, se esperaba DeadlineExceeded", err)
	}

	h.fah.set(func(f *fakeFAH) { f.delay = 0 })
	h.col.set(snapIdle, nil)
	deadline := time.Now().Add(6 * time.Second)
	for h.d.Status().Mode != ModeFolding {
		if time.Now().After(deadline) {
			t.Fatalf("el loop no se recuperó: %+v", h.d.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.d.Status().EcoWatts; got != 123 {
		t.Fatalf("un comando abandonado se ejecutó después: eco = %d", got)
	}
}
