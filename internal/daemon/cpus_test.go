package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
)

// cpuFAH is a fakeFAH that also limits CPU threads, like ClientV8 against
// FAH v8. cpus survives Pause and Resume.
type cpuFAH struct {
	*fakeFAH
	cmu    sync.Mutex
	cpus   int
	setErr error
}

var _ fah.CPULimiter = (*cpuFAH)(nil)

func (f *cpuFAH) SetCPUs(ctx context.Context, n int) error {
	f.log.add(fmt.Sprintf("fah.SetCPUs(%d)", n))
	f.cmu.Lock()
	defer f.cmu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.cpus = n
	return nil
}

func (f *cpuFAH) GetStatus(ctx context.Context) (fah.Status, error) {
	st, err := f.fakeFAH.GetStatus(ctx)
	if err == nil {
		f.cmu.Lock()
		st.CPUs = f.cpus
		f.cmu.Unlock()
	}
	return st, err
}

func (f *cpuFAH) setCPUs(fn func(f *cpuFAH)) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	fn(f)
}

func (f *cpuFAH) current() int {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	return f.cpus
}

// newCPUHarness is newHarness with a FAH that limits CPU threads and starts
// with the user's cpus.
func newCPUHarness(t *testing.T, userCPUs int) (*harness, *cpuFAH) {
	t.Helper()
	h := newHarness(t, false)
	cf := &cpuFAH{fakeFAH: h.fah, cpus: userCPUs}
	d, err := New(h.d.cfg, Deps{GPU: h.gpu, FAH: cf, Collector: h.col, Evaluator: h.eval})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	return h, cf
}

// Snapshots of a MacBook Air M4 (fanless: 1 thread) and a MacBook Pro M4
// (5 threads on AC, 1 at moderate pressure).
var (
	snapAirAC = detector.SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 90,
		MacProfile: detector.ProfileFanless, RecommendedCPUs: 1}
	snapProAC = detector.SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 90,
		MacProfile: detector.ProfileActiveLaptop, RecommendedCPUs: 5}
	snapProWarm = detector.SystemSnapshot{HasBattery: true, OnAC: true, BatteryPct: 90, ThermalLevel: 1,
		MacProfile: detector.ProfileActiveLaptop, RecommendedCPUs: 1}
)

// TestCPUsSetBeforeResume: the budget reaches FAH before Resume, the user's
// value is read first, and the status reports both.
func TestCPUsSetBeforeResume(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.start(t)
	h.ticks(1, func() detector.SystemSnapshot { s := snapAirAC; s.OnAC, s.BatteryPct = false, 10; return s }())
	if !h.d.pausedByUs {
		t.Fatal("no hubo pausa por batería")
	}
	h.log.reset()

	h.ticks(1, snapAirAC)
	set, resume := h.log.index("fah.SetCPUs(1)"), h.log.index("fah.Resume")
	if set < 0 || resume < 0 || set > resume {
		t.Fatalf("se esperaba SetCPUs(1) antes de Resume: %v", h.log.all())
	}
	if cf.current() != 1 || h.d.originalCPUs != 4 {
		t.Fatalf("cpus = %d, original = %d", cf.current(), h.d.originalCPUs)
	}
	h.d.refreshFAH(t.Context())
	st := h.d.publish()
	if st.FAH.CPUs != 1 || st.RecommendedCPUs != 1 || st.MacProfile != string(detector.ProfileFanless) {
		t.Fatalf("status = %+v", st)
	}

	// Already applied: no more calls.
	h.log.reset()
	h.ticks(10, snapAirAC)
	if h.log.count("fah.SetCPUs(1)") != 0 {
		t.Fatalf("SetCPUs repetido: %v", h.log.all())
	}
}

// TestCPUsLimitedWhenFAHAlreadyFolds (incident 2026-11-18): FAH folding at
// the user's 4 threads on a fanless Mac is limited on the first tick.
func TestCPUsLimitedWhenFAHAlreadyFolds(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.start(t)
	h.ticks(1, snapAirAC)
	if cf.current() != 1 || h.log.count("fah.Pause") != 0 || h.log.count("fah.Resume") != 0 {
		t.Fatalf("cpus = %d, %v", cf.current(), h.log.all())
	}
}

// TestCPUsFollowTheBudgetWhileFolding: a budget that changes with the
// thermal level is applied while FAH folds.
func TestCPUsFollowTheBudgetWhileFolding(t *testing.T) {
	h, cf := newCPUHarness(t, 10)
	h.start(t)
	h.ticks(1, snapProAC)
	if cf.current() != 5 {
		t.Fatalf("cpus = %d, se esperaban 5", cf.current())
	}
	h.ticks(1, snapProWarm)
	if cf.current() != 1 {
		t.Fatalf("a nivel 1: cpus = %d, se esperaba 1", cf.current())
	}
	h.ticks(1, snapProAC)
	if cf.current() != 5 || h.d.originalCPUs != 10 {
		t.Fatalf("cpus = %d, original = %d", cf.current(), h.d.originalCPUs)
	}
}

// TestCPUsReappliedAfterExternalChange: cpus changed in FAH's own UI while
// folding are put back to the budget after the next refresh.
func TestCPUsReappliedAfterExternalChange(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.start(t)
	h.ticks(1, snapAirAC)
	cf.setCPUs(func(f *cpuFAH) { f.cpus = 4 })
	h.d.fahAge = fahRefreshTicks
	h.d.refreshFAH(t.Context())
	h.ticks(1, snapAirAC)
	if cf.current() != 1 {
		t.Fatalf("cpus = %d, se esperaba 1", cf.current())
	}
	if h.d.originalCPUs != 4 {
		t.Fatalf("original = %d", h.d.originalCPUs)
	}
}

// TestCPUsFailureKeepsFoldingPaused: if the budget cannot be applied, the
// daemon's pause is not resumed until it can.
func TestCPUsFailureKeepsFoldingPaused(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.start(t)
	h.ticks(1, func() detector.SystemSnapshot { s := snapAirAC; s.OnAC, s.BatteryPct = false, 10; return s }())
	cf.setCPUs(func(f *cpuFAH) { f.setErr = errors.New("connection refused") })
	h.log.reset()

	h.ticks(5, snapAirAC)
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("reanudó sin limitar los hilos: %v", h.log.all())
	}
	if _, err := h.forceResume(); !errors.Is(err, ErrFAHUnavailable) {
		t.Fatalf("force_resume = %v, se esperaba ErrFAHUnavailable", err)
	}
	if h.log.count("fah.Resume") != 0 {
		t.Fatalf("force_resume reanudó sin limitar los hilos: %v", h.log.all())
	}

	cf.setCPUs(func(f *cpuFAH) { f.setErr = nil })
	h.ticks(1, snapAirAC)
	if set, resume := h.log.index("fah.SetCPUs(1)"), h.log.index("fah.Resume"); resume < 0 || cf.current() != 1 || set > resume {
		t.Fatalf("cpus = %d, %v", cf.current(), h.log.all())
	}
}

// TestForceResumeSetsCPUsFirst: force_resume applies the budget before
// Resume.
func TestForceResumeSetsCPUsFirst(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.start(t)
	h.col.set(snapAirAC, nil)
	if _, err := h.forcePause(); err != nil {
		t.Fatal(err)
	}
	h.d.recommendedCPUs = 1 // the evaluator saw the Mac on the first tick
	h.log.reset()
	if _, err := h.forceResume(); err != nil {
		t.Fatalf("force_resume: %v", err)
	}
	if set, resume := h.log.index("fah.SetCPUs(1)"), h.log.index("fah.Resume"); set < 0 || resume < 0 || set > resume {
		t.Fatalf("se esperaba SetCPUs(1) antes de Resume: %v", h.log.all())
	}
	if cf.current() != 1 {
		t.Fatalf("cpus = %d", cf.current())
	}
}

// TestCPUsNeverTouchedWithoutBudget: on Linux (RecommendedCPUs 0) FAH's cpus
// are never read for a change, set or restored.
func TestCPUsNeverTouchedWithoutBudget(t *testing.T) {
	h, cf := newCPUHarness(t, 8)
	h.col.set(snapIdle, nil)
	cancel, done := runHarness(t, h)
	waitFor(t, "ticks", func() bool { return h.col.callCount() > 5 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range h.log.all() {
		if strings.HasPrefix(c, "fah.SetCPUs") {
			t.Fatalf("llamada inesperada %q: %v", c, h.log.all())
		}
	}
	if cf.current() != 8 {
		t.Fatalf("cpus = %d", cf.current())
	}
}

// TestRunRestoresOriginalCPUs: on SIGTERM the user's cpus come back before
// FAH is closed, and folding is neither paused nor resumed.
func TestRunRestoresOriginalCPUs(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.col.set(snapAirAC, nil)
	cancel, done := runHarness(t, h)
	waitFor(t, "límite de hilos", func() bool { return cf.current() == 1 })
	h.log.reset()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	restore, closeIdx := h.log.index("fah.SetCPUs(4)"), h.log.index("fah.Close")
	if restore < 0 || closeIdx < 0 || restore > closeIdx {
		t.Fatalf("se esperaba SetCPUs(4) antes de Close: %v", h.log.all())
	}
	if cf.current() != 4 || h.log.count("fah.Pause") != 0 || h.log.count("fah.Resume") != 0 {
		t.Fatalf("cpus = %d, %v", cf.current(), h.log.all())
	}
}

// TestRunReportsFailedCPUsRestore: a restore that fails is an error of Run.
func TestRunReportsFailedCPUsRestore(t *testing.T) {
	h, cf := newCPUHarness(t, 4)
	h.col.set(snapAirAC, nil)
	cancel, done := runHarness(t, h)
	waitFor(t, "límite de hilos", func() bool { return cf.current() == 1 })
	cf.setCPUs(func(f *cpuFAH) { f.setErr = errors.New("connection refused") })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run no informó la restauración fallida")
	}
}
