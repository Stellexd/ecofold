package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellexd/ecofold/internal/fah"
)

func (h *harness) markerOrigin(t *testing.T) pauseOrigin {
	t.Helper()
	origin, ok, err := marker{dir: h.dir}.read()
	if err != nil || !ok {
		t.Fatalf("marcador: ok=%v, err=%v", ok, err)
	}
	return origin
}

// TestMarkerOriginRoundTrip: the origin is written and read back; an empty
// or unknown content (a marker from an older build) reads as auto.
func TestMarkerOriginRoundTrip(t *testing.T) {
	m := marker{dir: t.TempDir()}
	if _, ok, err := m.read(); ok || err != nil {
		t.Fatalf("sin marcador: ok=%v, err=%v", ok, err)
	}
	for _, origin := range []pauseOrigin{originManual, originAuto, originManual} {
		if err := m.set(origin); err != nil {
			t.Fatalf("set(%s): %v", origin, err)
		}
		got, ok, err := m.read()
		if err != nil || !ok || got != origin {
			t.Fatalf("read tras set(%s) = %q, %v, %v", origin, got, ok, err)
		}
		raw, _ := os.ReadFile(m.path())
		if string(raw) != string(origin)+"\n" {
			t.Fatalf("contenido = %q", raw)
		}
		if fi, _ := os.Stat(m.path()); fi.Mode().Perm() != 0o600 {
			t.Fatalf("permisos = %v", fi.Mode().Perm())
		}
	}
	for _, legacy := range []string{"", "\n", "MANUAL", "manualx", "pause", "\x00\xff"} {
		if err := os.WriteFile(m.path(), []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := m.read(); err != nil || !ok || got != originAuto {
			t.Fatalf("contenido %q leído como %q, %v, %v; se esperaba auto", legacy, got, ok, err)
		}
	}
	if err := os.WriteFile(m.path(), []byte("  manual \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := m.read(); got != originManual {
		t.Fatalf("manual con espacios leído como %q", got)
	}
}

// TestMarkerReadDoesNotFollowSymlinks: a symlink at the marker path counts as
// present but its target is never read, so a link to a file that says
// "manual" cannot pin folding in a manual pause.
func TestMarkerReadDoesNotFollowSymlinks(t *testing.T) {
	m := marker{dir: t.TempDir()}
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("manual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, m.path()); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := m.read(); err != nil || !ok || got != originAuto {
		t.Fatalf("symlink leído como %q, %v, %v; se esperaba auto sin seguirlo", got, ok, err)
	}
	if err := m.set(originAuto); err != nil {
		t.Fatalf("set: %v", err)
	}
	if fi, err := os.Lstat(m.path()); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("el marcador no reemplazó el symlink (err=%v)", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "manual\n" {
		t.Fatalf("el destino del symlink cambió: %q", got)
	}
}

// TestForcePauseWritesManualMarker: a manual pause is recorded as manual, an
// automatic one as auto, and a manual pause over an automatic one upgrades
// the marker.
func TestForcePauseWritesManualMarker(t *testing.T) {
	h := newHarness(t, false)
	h.start(t)
	h.ticks(1, snapIdle)
	if _, err := h.forcePause(); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	if got := h.markerOrigin(t); got != originManual {
		t.Fatalf("origen = %q, se esperaba manual", got)
	}

	h = newHarness(t, false)
	h.start(t)
	h.ticks(1, snapLowBattery)
	if got := h.markerOrigin(t); got != originAuto {
		t.Fatalf("pausa por batería: origen = %q", got)
	}
	if _, err := h.forcePause(); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	if h.log.count("fah.Pause") != 1 {
		t.Fatalf("Pause repetido: %v", h.log.all())
	}
	if got := h.markerOrigin(t); got != originManual {
		t.Fatalf("force_pause sobre una pausa propia: origen = %q", got)
	}

	// A protective pause during the manual one never downgrades it.
	h.ticks(sustainTicks, snapLoad)
	if got := h.markerOrigin(t); got != originManual {
		t.Fatalf("una pausa por carga degradó el marcador a %q", got)
	}

	// force_resume during the load pause clears the override: what remains is
	// an automatic pause.
	if _, err := h.forceResume(); err == nil {
		t.Fatal("force_resume debía fallar durante la carga")
	}
	if got := h.markerOrigin(t); got != originAuto {
		t.Fatalf("tras force_resume: origen = %q, se esperaba auto", got)
	}
}

// TestManualPauseSurvivesRestart (incident 2026-11-18, 3.5): a daemon that
// restarts over a manual marker starts in paused_manual and does not resume
// FAH when the startup hold would have ended, however long it runs.
func TestManualPauseSurvivesRestart(t *testing.T) {
	h := newHarness(t, true)
	if err := (marker{dir: h.dir}).set(originManual); err != nil {
		t.Fatal(err)
	}
	h.userPausesFAH()
	h.start(t)

	st := h.d.Status()
	if st.Mode != ModePausedManual || st.ManualOverride != OverridePause || !st.PausedByUs || st.StartupHold {
		t.Fatalf("status al arrancar = %+v", st)
	}
	h.ticks(2*DefaultConfig().StartupHoldTicks+5, snapIdle)
	h.d.refreshFAH(t.Context())
	if h.log.count("fah.Resume") != 0 || h.log.count("gpu.Set(123)") != 0 {
		t.Fatalf("se reanudó una pausa manual tras reiniciar: %v", h.log.all())
	}
	st = h.d.publish()
	if st.Mode != ModePausedManual || st.ManualOverride != OverridePause || !st.PausedByUs || st.StartupHold {
		t.Fatalf("status tras la espera = %+v", st)
	}
	if got := h.markerOrigin(t); got != originManual {
		t.Fatalf("origen = %q", got)
	}

	if _, err := h.forceResume(); err != nil {
		t.Fatalf("force_resume: %v", err)
	}
	set, resume := h.log.index("gpu.Set(123)"), h.log.index("fah.Resume")
	if set < 0 || resume < 0 || set > resume {
		t.Fatalf("se esperaba Set(123) y luego Resume: %v", h.log.all())
	}
	if h.markerPresent(t) || h.d.pausedByUs {
		t.Fatal("la pausa siguió siendo del daemon tras force_resume")
	}
}

// TestAutoMarkerStillResumesAfterHold: an automatic (or legacy, empty) marker
// keeps the old behavior: resume after the startup hold.
func TestAutoMarkerStillResumesAfterHold(t *testing.T) {
	for name, content := range map[string]string{"auto": "auto\n", "legado vacío": ""} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, false)
			if err := os.WriteFile(filepath.Join(h.dir, markerName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			h.fah.set(func(f *fakeFAH) { f.status = fah.Status{Paused: true, State: fah.StatePaused} })
			h.start(t)
			if st := h.d.Status(); st.ManualOverride != OverrideNone || !st.StartupHold {
				t.Fatalf("status al arrancar = %+v", st)
			}
			h.ticks(DefaultConfig().StartupHoldTicks, snapIdle)
			if h.log.count("fah.Resume") != 0 {
				t.Fatal("reanudó antes de terminar la espera")
			}
			h.ticks(1, snapIdle)
			if h.log.count("fah.Resume") != 1 || h.markerPresent(t) {
				t.Fatalf("no reanudó tras la espera: %v", h.log.all())
			}
		})
	}
}
