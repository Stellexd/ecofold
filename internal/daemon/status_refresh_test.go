package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
)

// TestStatusReportsFAHWithoutAPause: a daemon that starts folding never
// pauses or resumes, and must still report what FAH is doing.
func TestStatusReportsFAHWithoutAPause(t *testing.T) {
	h := newHarness(t, true)
	if st := h.d.Status(); st.FAH.State != fah.StateUnknown || !st.FAH.ObservedAt.IsZero() {
		t.Fatalf("antes de arrancar: %+v", st.FAH)
	}
	cancel, done := h.runLoop(t)
	defer func() { cancel(); <-done }()

	waitFor(t, "estado de FAH", func() bool {
		f := h.d.Status().FAH
		return f.Reachable && f.State == fah.StateFolding && f.ActiveProjectID == 18264 && !f.Paused
	})
	if h.log.count("fah.Pause") != 0 || h.log.count("fah.Resume") != 0 {
		t.Fatalf("el refresco tocó FAH: %v", h.log.all())
	}

	// A pause made in FAH's own UI shows up by itself, and stays the user's.
	h.userPausesFAH()
	waitFor(t, "pausa del usuario", func() bool {
		st := h.d.Status()
		return st.FAH.Paused && st.FAH.State == fah.StatePaused && st.FAH.ActiveProjectID == 0 && !st.PausedByUs
	})

	h.fah.set(func(f *fakeFAH) { f.status = fah.Status{State: fah.StateIdle} })
	waitFor(t, "idle", func() bool { return h.d.Status().FAH.State == fah.StateIdle })

	// FAH goes away: unreachable, with the last known state kept.
	h.fah.set(func(f *fakeFAH) { f.statusErr = errors.New("connection refused") })
	waitFor(t, "FAH caído", func() bool { return !h.d.Status().FAH.Reachable })
	if f := h.d.Status().FAH; f.State != fah.StateIdle || f.ObservedAt.IsZero() {
		t.Fatalf("se perdió el último estado conocido: %+v", f)
	}
	h.fah.set(func(f *fakeFAH) { f.statusErr = nil })
	waitFor(t, "FAH de vuelta", func() bool { return h.d.Status().FAH.Reachable })
}

// TestFAHRefreshCadence: the status is read once per fahRefreshTicks loop
// passes, and a pause or resume counts as an observation.
func TestFAHRefreshCadence(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	ctx := context.Background()

	h.d.refreshFAH(ctx) // first pass
	if got := h.log.count("fah.Status"); got != 1 {
		t.Fatalf("primer pase: %d lecturas", got)
	}
	for i := 0; i < fahRefreshTicks-1; i++ {
		h.d.refreshFAH(ctx)
	}
	if got := h.log.count("fah.Status"); got != 1 {
		t.Fatalf("lecturas antes de tiempo: %d", got)
	}
	h.d.refreshFAH(ctx)
	if got := h.log.count("fah.Status"); got != 2 {
		t.Fatalf("tras %d pases: %d lecturas", fahRefreshTicks, got)
	}

	// A confirmed pause is an observation: paused, no project.
	h.ticks(sustainTicks, snapLoad)
	if f := h.d.publish().FAH; !f.Paused || f.State != fah.StatePaused || f.ActiveProjectID != 0 || !f.Reachable {
		t.Fatalf("tras la pausa: %+v", f)
	}

	// A resume does not say what FAH does next: the next pass reads it.
	h.ticks(cooldownTicks+1, snapIdle)
	if h.log.count("fah.Resume") != 1 {
		t.Fatalf("no se reanudó: %v", h.log.all())
	}
	h.log.reset()
	h.d.refreshFAH(ctx)
	if f := h.d.publish().FAH; h.log.count("fah.Status") != 1 || f.State != fah.StateFolding || f.ActiveProjectID != 18264 {
		t.Fatalf("tras reanudar: lecturas=%d fah=%+v", h.log.count("fah.Status"), f)
	}
}

// TestStatusNeverPublishesSimulatedTelemetry: a mock provider's watts,
// temperature and VRAM are not the machine's; the name and the device-wide
// metrics come from the collector.
func TestStatusNeverPublishesSimulatedTelemetry(t *testing.T) {
	h := newHarness(t, false)
	h.gpu.set(func(g *fakeGPU) {
		g.tel.Name, g.tel.Simulated = "Mock NVIDIA GeForce RTX 5060", true
		g.tel.TempC, g.tel.PowerUsageW, g.tel.UsedVRAMMiB, g.tel.TotalVRAMMiB = 42, 75, 512, 8192
	})
	h.start(t)

	h.ticks(1, detector.SystemSnapshot{OnAC: true, GPUClass: "AGXAcceleratorG16G", GPUModel: "Apple M4",
		GPUDeviceUtilPct: 24, GPUInUseMemMiB: 1234})
	want := GPUStatus{Name: "Apple M4", UtilPct: 24, InUseMemMiB: 1234}
	if got := h.d.publish().GPU; got != want {
		t.Fatalf("gpu = %+v, se esperaba %+v", got, want)
	}

	// Without a model the class is the name.
	h.ticks(1, detector.SystemSnapshot{OnAC: true, GPUClass: "AGXAcceleratorG16G", GPUDeviceUtilPct: 3})
	want = GPUStatus{Name: "AGXAcceleratorG16G", UtilPct: 3}
	if got := h.d.publish().GPU; got != want {
		t.Fatalf("gpu = %+v, se esperaba %+v", got, want)
	}
}

// TestStatusKeepsRealTelemetry: a real device's telemetry is published as
// read, whatever the snapshot calls the GPU.
func TestStatusKeepsRealTelemetry(t *testing.T) {
	h := newHarness(t, true)
	h.gpu.set(func(g *fakeGPU) {
		g.tel.TempC, g.tel.PowerUsageW, g.tel.UsedVRAMMiB, g.tel.TotalVRAMMiB = 61, 118, 812, 8151
	})
	h.start(t)
	h.ticks(1, detector.SystemSnapshot{OnAC: true, GPUClass: "otro nombre"})
	want := GPUStatus{Name: "Fake RTX 5060", TempC: 61, PowerUsageW: 118, UsedVRAMMiB: 812, TotalVRAMMiB: 8151}
	if got := h.d.publish().GPU; got != want {
		t.Fatalf("gpu = %+v, se esperaba %+v", got, want)
	}
}
