package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// The chaos model drives the daemon with random event sequences and checks
// invariants after every step. The fakes below keep a ground-truth model of
// the GPU limit and of who paused FAH, which the daemon cannot see.

const (
	modelDefaultW = 145
	modelEcoW     = 123
)

// modelGPU tracks the real power limit.
type modelGPU struct {
	mu                sync.Mutex
	limit             int
	setErr            error
	restoreErr        error
	lastRestoreFailed bool
	ops               []string
}

func (g *modelGPU) Init() error     { return nil }
func (g *modelGPU) Shutdown() error { return nil }
func (g *modelGPU) GetTelemetry() (gpu.Telemetry, error) {
	return gpu.Telemetry{
		Name: "model", SupportsPowerLimit: true,
		MinPowerLimitW: modelEcoW, MaxPowerLimitW: modelDefaultW, DefaultPowerLimitW: modelDefaultW,
	}, nil
}

func (g *modelGPU) SetPowerLimit(w int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops = append(g.ops, "set")
	if g.setErr != nil {
		return g.setErr
	}
	g.limit = w
	return nil
}

func (g *modelGPU) RestoreDefaultPowerLimit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops = append(g.ops, "restore")
	if g.restoreErr != nil {
		g.lastRestoreFailed = true
		return g.restoreErr
	}
	g.lastRestoreFailed = false
	g.limit = modelDefaultW
	return nil
}

func (g *modelGPU) GetGraphicsProcesses() ([]gpu.ProcessInfo, error) { return nil, nil }

// modelFAH tracks who paused folding.
type modelFAH struct {
	mu        sync.Mutex
	pausedBy  string // "", "user", "daemon" or "user-claimed" (documented gap)
	statusErr error
	actionErr error

	// Filled by the daemon calls, checked by the harness.
	resumeWhileUserPaused bool
	resumeLimit           []int
	pauseCalls            int
	resumeCalls           int
	gpu                   *modelGPU
}

func (f *modelFAH) GetStatus(ctx context.Context) (fah.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return fah.Status{State: fah.StateUnknown}, f.statusErr
	}
	if f.pausedBy != "" {
		return fah.Status{Paused: true, State: fah.StatePaused}, nil
	}
	return fah.Status{ActiveProjectID: 1, State: fah.StateFolding}, nil
}

func (f *modelFAH) Pause(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	if f.actionErr != nil {
		return f.actionErr
	}
	switch f.pausedBy {
	case "":
		f.pausedBy = "daemon"
	case "user":
		// Only reachable when GetStatus failed (CH2-H1 + CH-10 gap in
		// DAEMON_PLAN.md F4): the daemon now owns the user's pause.
		f.pausedBy = "user-claimed"
	}
	return nil
}

func (f *modelFAH) Resume(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	if f.pausedBy == "user" {
		f.resumeWhileUserPaused = true
	}
	f.gpu.mu.Lock()
	f.resumeLimit = append(f.resumeLimit, f.gpu.limit)
	f.gpu.mu.Unlock()
	if f.actionErr != nil {
		return f.actionErr
	}
	f.pausedBy = ""
	return nil
}

func (f *modelFAH) Close() error { return nil }

// userToggle simulates the user pausing or resuming by hand. A user pause on
// top of a daemon pause keeps the daemon's ownership (documented gap).
func (f *modelFAH) userToggle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.pausedBy {
	case "":
		f.pausedBy = "user"
	case "user":
		f.pausedBy = ""
	}
}

// chaosRun is one model run.
type chaosRun struct {
	t   testing.TB
	dir string
	gpu *modelGPU
	fah *modelFAH
	col *scriptedCollector
	d   *Daemon
}

func newChaosRun(t testing.TB) *chaosRun {
	t.Helper()
	g := &modelGPU{limit: modelDefaultW}
	r := &chaosRun{
		t:   t,
		gpu: g,
		fah: &modelFAH{gpu: g},
		col: &scriptedCollector{snap: snapIdle},
	}
	r.dir = t.TempDir()
	r.restart(false)
	return r
}

// restart builds a fresh daemon over the same hardware, FAH and marker dir.
// clean runs the exit fail-safe of the previous instance first.
func (r *chaosRun) restart(clean bool) {
	if r.d != nil && clean {
		_ = r.d.failSafe(false)
		r.checkExit()
	}
	cfg := DefaultConfig()
	cfg.RuntimeDir = r.dir
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg, Deps{GPU: r.gpu, FAH: r.fah, Collector: r.col, Evaluator: detector.NewEvaluator(detector.DefaultConfig())})
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	if err := d.start(); err != nil {
		r.t.Fatalf("start: %v", err)
	}
	r.d = d
}

func (r *chaosRun) tick(snap detector.SystemSnapshot, err error) {
	inherited := r.d.resumeHold > 0 && r.d.pausedByUs && !r.d.degraded
	r.gpu.mu.Lock()
	opsBefore := len(r.gpu.ops)
	r.gpu.mu.Unlock()
	r.fah.mu.Lock()
	resumesBefore := r.fah.resumeCalls
	r.fah.mu.Unlock()

	r.col.set(snap, err)
	r.d.tick(context.Background())

	if inherited {
		r.gpu.mu.Lock()
		ops := append([]string(nil), r.gpu.ops[opsBefore:]...)
		r.gpu.mu.Unlock()
		for _, op := range ops {
			// I7: an inherited pause keeps the default limit during the
			// startup hold; a game may still be running.
			if op == "set" {
				r.t.Fatalf("I7: SetPowerLimit durante la espera de arranque con pausa heredada")
			}
		}
		r.fah.mu.Lock()
		resumed := r.fah.resumeCalls > resumesBefore
		r.fah.mu.Unlock()
		// I8: an inherited pause is never resumed before the hold ends.
		if resumed {
			r.t.Fatalf("I8: Resume durante la espera de arranque con pausa heredada")
		}
	}
	r.checkTick()
}

// checkTick asserts the invariants that must hold after every tick.
func (r *chaosRun) checkTick() {
	r.t.Helper()
	g, f, d := r.gpu, r.fah, r.d
	g.mu.Lock()
	limit, lastRestoreFailed := g.limit, g.lastRestoreFailed
	g.mu.Unlock()
	f.mu.Lock()
	resumeWhileUser, resumeLimits := f.resumeWhileUserPaused, append([]int(nil), f.resumeLimit...)
	f.resumeLimit = nil
	f.mu.Unlock()

	// I1 (CH-10): a pause the user made is never resumed.
	if resumeWhileUser {
		r.t.Fatalf("I1: Resume con una pausa manual del usuario activa")
	}
	// I2 (F7): Resume only runs with the eco cap in place, unless power
	// control is off (macOS, NO_PERMISSION or Degraded).
	if d.powerEnabled {
		for _, l := range resumeLimits {
			if l != modelEcoW {
				r.t.Fatalf("I2: Resume con el límite en %d W, se esperaba el tope de %d W", l, modelEcoW)
			}
		}
	}
	// I3: the daemon never loses track of a cap it applied.
	if limit == modelEcoW && !d.capApplied {
		r.t.Fatalf("I3: límite real %d W pero capApplied=false", limit)
	}
	// I4: while paused, the GPU is at the default limit unless the last
	// restore attempt failed.
	if d.haveDecision && isPause(d.lastDecision) && limit != modelDefaultW && !lastRestoreFailed && d.collectErrs == 0 {
		r.t.Fatalf("I4: decisión %s con el límite en %d W", d.lastDecision, limit)
	}
	// I5: ownership in memory matches the marker on disk.
	present, err := d.marker.present()
	if err != nil {
		r.t.Fatalf("marker.present: %v", err)
	}
	if present != d.pausedByUs {
		r.t.Fatalf("I5: marcador en disco=%v, pausedByUs=%v", present, d.pausedByUs)
	}
}

// checkExit asserts the fail-safe left the GPU at the default limit.
func (r *chaosRun) checkExit() {
	r.t.Helper()
	r.gpu.mu.Lock()
	defer r.gpu.mu.Unlock()
	if r.gpu.restoreErr == nil && r.gpu.limit != modelDefaultW {
		r.t.Fatalf("I6: el límite quedó en %d W tras el apagado", r.gpu.limit)
	}
}

// step applies one event encoded in b. The low nibble picks the event, the
// high nibble repeats tick events (1-16 times) so cooldowns are reachable.
func (r *chaosRun) step(b byte) {
	reps := int(b>>4) + 1
	switch b & 0x0f {
	case 0, 1, 2, 3:
		for i := 0; i < reps*4; i++ {
			r.tick(snapIdle, nil)
		}
	case 4, 5, 6:
		for i := 0; i < reps; i++ {
			r.tick(snapLoad, nil)
		}
	case 7:
		r.tick(snapLowBattery, nil)
	case 8:
		r.tick(snapBatteryOK, nil)
	case 9:
		for i := 0; i < reps; i++ {
			r.tick(detector.SystemSnapshot{}, errors.New("collect: transient"))
		}
	case 10:
		switch {
		case b>>4 == 0x0f: // rare: unsupported per-process utilization
			r.tick(detector.SystemSnapshot{}, detector.ErrPerProcessUtilUnsupported)
		case b&0x10 != 0: // game in its session, with load
			for i := 0; i < reps; i++ {
				r.tick(snapGameLoad, nil)
			}
		default: // same game idle in a menu, session still live
			for i := 0; i < reps*4; i++ {
				r.tick(snapGameMenu, nil)
			}
		}
	case 11:
		r.fah.mu.Lock()
		if b&0x10 != 0 {
			r.fah.statusErr = toggle(r.fah.statusErr, "fah: status timeout")
		} else {
			r.fah.actionErr = toggle(r.fah.actionErr, "fah: connection refused")
		}
		r.fah.mu.Unlock()
	case 12:
		r.gpu.mu.Lock()
		switch {
		case b&0x30 == 0x30:
			r.gpu.setErr = toggleErr(r.gpu.setErr, fmt.Errorf("SetPowerManagementLimit failed: %w", gpu.ErrNoPermission))
		case b&0x10 != 0:
			r.gpu.setErr = toggle(r.gpu.setErr, "nvml: set timeout")
		default:
			r.gpu.restoreErr = toggle(r.gpu.restoreErr, "nvml: restore timeout")
		}
		r.gpu.mu.Unlock()
	case 13:
		r.fah.userToggle()
	case 14:
		r.restart(b&0x10 != 0)
	case 15:
		r.tick(snapLoad, nil)
	}
}

const modelSession = "steam:1422450/90/1000"

var (
	snapGameLoad = detector.SystemSnapshot{
		OnAC:               true,
		GPUProcesses:       []gpu.ProcessInfo{{PID: 100, UsedVRAMMiB: 4096, SMUtilPct: 60, SMUtilValid: true}},
		GPUProcessSessions: map[uint32]string{100: modelSession},
		LiveGameSessions:   []string{modelSession},
	}
	snapGameMenu = detector.SystemSnapshot{
		OnAC:               true,
		GPUProcesses:       []gpu.ProcessInfo{{PID: 100, UsedVRAMMiB: 512, SMUtilPct: 3, SMUtilValid: true}},
		GPUProcessSessions: map[uint32]string{100: modelSession},
		LiveGameSessions:   []string{modelSession},
	}
)

func toggle(err error, msg string) error {
	if err != nil {
		return nil
	}
	return errors.New(msg)
}

func toggleErr(err, next error) error {
	if err != nil {
		return nil
	}
	return next
}

// TestChaosModelRandomSequences runs random event sequences against the
// invariants. Seeds are fixed so a failure is reproducible.
func TestChaosModelRandomSequences(t *testing.T) {
	seeds := 100
	if testing.Short() {
		seeds = 30
	}
	for seed := int64(0); seed < int64(seeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		events := make([]byte, 400)
		rng.Read(events)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := newChaosRun(t)
			for _, b := range events {
				r.step(b)
			}
			r.gpu.mu.Lock()
			r.gpu.restoreErr = nil
			r.gpu.mu.Unlock()
			_ = r.d.failSafe(false)
			r.checkExit()
		})
	}
}

// FuzzDaemonInvariants explores event sequences with the native fuzzer.
func FuzzDaemonInvariants(f *testing.F) {
	f.Add([]byte{0x24, 0x24, 0x24, 0xf0, 0xf0, 0xf0, 0xf0})       // load → cooldown
	f.Add([]byte{0x0d, 0x24, 0x24, 0xf0, 0xf0, 0xf0, 0xf0, 0x0d}) // user pause
	f.Add([]byte{0x1b, 0x0d, 0x24, 0x24, 0x1b, 0xf0, 0xf0, 0xf0}) // status error + user pause
	f.Add([]byte{0x24, 0x24, 0x1e, 0xf0, 0xf0, 0xf0, 0xf0})       // restart while paused
	f.Add([]byte{0x3c, 0x07, 0x00, 0x24, 0x24, 0x3c})             // NO_PERMISSION
	f.Add([]byte{0x0c, 0x07, 0x00, 0x0c, 0x0e, 0x07})             // restore failures + crash
	f.Add([]byte{0x24, 0x24, 0xfa, 0xf0, 0x07, 0x00})             // Degraded while paused
	f.Add([]byte{0x1a, 0x1a, 0x1a, 0xea, 0x0e, 0x0a, 0xf0})       // game session, crash in the menu
	f.Fuzz(func(t *testing.T, events []byte) {
		if len(events) > 512 {
			events = events[:512]
		}
		r := newChaosRun(t)
		for _, b := range events {
			r.step(b)
		}
		r.gpu.mu.Lock()
		r.gpu.restoreErr = nil
		r.gpu.mu.Unlock()
		_ = r.d.failSafe(false)
		r.checkExit()
	})
}
