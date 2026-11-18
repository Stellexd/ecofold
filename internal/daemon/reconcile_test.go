package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
)

// externalResume simulates FAH folding again behind the daemon's back (its
// own UI, or another client).
func (h *harness) externalResume() {
	h.fah.set(func(f *fakeFAH) { f.status = fah.Status{ActiveProjectID: 18803, State: fah.StateFolding} })
}

// refreshNow makes the next refreshFAH read FAH.
func (h *harness) refreshNow(t *testing.T) {
	t.Helper()
	h.d.fahAge = fahRefreshTicks
	h.d.refreshFAH(t.Context())
}

// TestExternalResumeDuringProtectivePauseRepauses (incident 2026-11-18, 3.3):
// while a load, battery or thermal reason holds, a resume made outside
// EcoFold is undone on the next refresh.
func TestExternalResumeDuringProtectivePauseRepauses(t *testing.T) {
	for _, tc := range []struct {
		snap  detector.SystemSnapshot
		ticks int
		mode  Mode
	}{
		{snapLoad, sustainTicks, ModePausedLoad},
		{snapLowBattery, 1, ModePausedBattery},
		{snapHot, 1, ModePausedThermal},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			h := newHarness(t, false)
			h.start(t)
			h.ticks(tc.ticks, tc.snap)
			if h.d.mode != tc.mode || !h.d.pausedByUs || h.log.count("fah.Pause") != 1 {
				t.Fatalf("modo = %s, pausedByUs = %v, %v", h.d.mode, h.d.pausedByUs, h.log.all())
			}

			h.externalResume()
			h.refreshNow(t)
			if h.log.count("fah.Pause") != 2 {
				t.Fatalf("no se volvió a pausar en el mismo pase: %v", h.log.all())
			}
			st := h.d.publish()
			if !st.PausedByUs || !st.FAH.Paused || st.Mode != tc.mode || !h.markerPresent(t) {
				t.Fatalf("status = %+v", st)
			}
			if got := h.markerOrigin(t); got != originAuto {
				t.Fatalf("origen = %q", got)
			}
			if fst, _ := h.fah.GetStatus(t.Context()); !fst.Paused {
				t.Fatal("FAH sigue plegando")
			}
		})
	}
}

// TestExternalResumeRepauseRetried: if the new pause fails, it stays pending
// and the next tick retries it.
func TestExternalResumeRepauseRetried(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)
	h.ticks(1, snapLowBattery)
	h.externalResume()
	h.fah.set(func(f *fakeFAH) { f.pauseErr = errors.New("connection refused") })
	h.refreshNow(t)
	if h.d.pending != opPause {
		t.Fatalf("pending = %v, se esperaba opPause", h.d.pending)
	}
	h.fah.set(func(f *fakeFAH) { f.pauseErr = nil })
	h.ticks(1, snapLowBattery)
	if fst, _ := h.fah.GetStatus(t.Context()); !fst.Paused || !h.d.pausedByUs {
		t.Fatal("el reintento no volvió a pausar")
	}
}

// TestExternalResumeClearsManualPause (CH-10): a resume made in FAH during a
// manual pause is the user's. The override, ownership and marker go, the
// status reports folding, and the daemon never pauses again on its own.
func TestExternalResumeClearsManualPause(t *testing.T) {
	h := newHarness(t, true)
	h.start(t)
	h.ticks(1, snapIdle)
	if _, err := h.forcePause(); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	h.log.reset()

	h.externalResume()
	h.refreshNow(t)
	st := h.d.publish()
	if st.ManualOverride != OverrideNone || st.PausedByUs || st.Mode != ModeFolding || st.FAH.Paused ||
		st.FAH.State != fah.StateFolding {
		t.Fatalf("status = %+v", st)
	}
	if h.markerPresent(t) {
		t.Fatal("el marcador sigue en disco")
	}
	if !strings.Contains(h.logs.String(), "manual pause cleared") {
		t.Fatal("falta el registro de la pausa manual liberada")
	}

	h.ticks(cooldownTicks, snapIdle)
	if h.log.count("fah.Pause") != 0 || h.log.count("fah.Resume") != 0 {
		t.Fatalf("el daemon actuó sobre la reanudación del usuario: %v", h.log.all())
	}
	if h.log.count("gpu.Set(123)") != 1 {
		t.Fatalf("FAH pliega y el tope eco no se aplicó: %v", h.log.all())
	}
}

// TestManualMarkerWithFoldingFAHAfterRestart: the user resumed FAH while the
// daemon was down; the first refresh hands the pause back to the user.
func TestManualMarkerWithFoldingFAHAfterRestart(t *testing.T) {
	h := newHarness(t, false)
	if err := (marker{dir: h.dir}).set(originManual); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	if h.d.manual != OverridePause {
		t.Fatal("no se restauró la pausa manual")
	}
	h.ticks(1, snapIdle)
	h.d.refreshFAH(t.Context()) // the first loop pass reads FAH
	st := h.d.publish()
	if st.ManualOverride != OverrideNone || st.PausedByUs || st.Mode != ModeFolding || h.markerPresent(t) {
		t.Fatalf("status = %+v", st)
	}
	if h.log.count("fah.Pause") != 0 {
		t.Fatalf("se pausó una reanudación del usuario: %v", h.log.all())
	}
}

// TestUserPauseStillUntouchedByRefresh: the refresh only acts on the
// daemon's own pause; a user's pause and FAH folding without a pause are
// left alone.
func TestUserPauseStillUntouchedByRefresh(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)
	h.ticks(1, snapIdle)
	h.refreshNow(t)
	h.userPausesFAH()
	h.refreshNow(t)
	h.ticks(cooldownTicks, snapIdle)
	h.refreshNow(t)
	if h.log.count("fah.Pause") != 0 || h.log.count("fah.Resume") != 0 {
		t.Fatalf("el refresco tocó FAH: %v", h.log.all())
	}
}
