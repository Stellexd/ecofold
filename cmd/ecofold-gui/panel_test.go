//go:build cgo

package main

import (
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/Stellexd/ecofold/internal/gui"
)

// recorder captures what the panel asks for.
type recorder struct {
	pauses, resumes int
	watts           []int
	lists           []string
}

func (r *recorder) actions() actions {
	return actions{
		pause:        func() { r.pauses++ },
		resume:       func() { r.resumes++ },
		setEcoWatts:  func(w int) { r.watts = append(r.watts, w) },
		setPauseList: func(s string) { r.lists = append(r.lists, s) },
	}
}

// newTestPanel builds the panel on Fyne's software driver: no display.
func newTestPanel(t *testing.T) (*panel, *recorder, fyne.Window) {
	t.Helper()
	a := test.NewApp()
	t.Cleanup(a.Quit)
	rec := &recorder{}
	p := newPanel(rec.actions())
	w := test.NewWindow(p.root)
	w.Resize(fyne.NewSize(520, 680))
	t.Cleanup(w.Close)
	return p, rec, w
}

// linuxView is a desktop with an NVIDIA GPU, folding under the eco limit.
func linuxView() gui.View {
	var s gui.Status
	s.Mode = gui.ModeFolding
	s.ManualOverride = "none"
	s.PowerCapable, s.PowerEnabled, s.CapApplied = true, true, true
	s.PowerLimitW, s.DefaultPowerLimitW, s.MinPowerLimitW, s.MaxPowerLimitW, s.EcoWatts = 123, 145, 100, 145, 123
	s.GPU.Name, s.GPU.TempC, s.GPU.PowerUsageW, s.GPU.UsedVRAMMiB, s.GPU.TotalVRAMMiB = "NVIDIA GeForce RTX 5060", 61, 118, 812, 8151
	s.Battery.OnAC = true
	s.FAH.Reachable, s.FAH.ObservedAt = true, time.Now()
	s.FAH.State, s.FAH.ActiveProjectID = "folding", 18264
	s.PauseListSupported = true
	s.PauseProcesses = []string{"blender", "obs*"}
	s.UpdatedAt = time.Date(2026, 11, 16, 15, 4, 5, 0, time.Local)
	return gui.View{Connected: true, Status: s}
}

// macView is an Apple Silicon laptop: no power limit, no pause list, a
// battery.
func macView() gui.View {
	v := linuxView()
	s := &v.Status
	s.Mode = gui.ModeFoldingEco
	s.PowerCapable, s.PowerEnabled, s.CapApplied = false, false, false
	s.EcoWatts = 0
	s.GPU.Name, s.GPU.TempC, s.GPU.PowerUsageW, s.GPU.UsedVRAMMiB, s.GPU.TotalVRAMMiB = "Apple M4", 0, 0, 0, 0
	s.GPU.UtilPct, s.GPU.InUseMemMiB = 24, 1234
	s.Battery.Present, s.Battery.OnAC, s.Battery.Pct = true, false, 53
	s.PauseListSupported = false
	s.PauseProcesses = nil
	return v
}

func TestPanelOnLinux(t *testing.T) {
	p, _, _ := newTestPanel(t)
	p.update(linuxView())

	if p.mode.Text != "Folding" || p.fah.Text != "Folding@home: running (project #18264)" {
		t.Fatalf("cabecera = %q / %q", p.mode.Text, p.fah.Text)
	}
	if p.gpuName.Text != "NVIDIA GeForce RTX 5060" || p.usage.Text != "Drawing 118 W · 61 °C · VRAM 812 / 8151 MiB" {
		t.Fatalf("gpu = %q / %q", p.gpuName.Text, p.usage.Text)
	}
	if p.badge.Visible() || p.offline.Visible() || p.sessions.Visible() {
		t.Fatal("badge, aviso offline o sesiones visibles sin motivo")
	}
	if !p.ecoBox.Visible() || p.ecoSlide.Min != 100 || p.ecoSlide.Max != 145 || p.ecoSlide.Value != 123 {
		t.Fatalf("control eco: visible=%v rango=%v-%v valor=%v", p.ecoBox.Visible(), p.ecoSlide.Min, p.ecoSlide.Max, p.ecoSlide.Value)
	}
	if !p.ecoApply.Disabled() {
		t.Fatal("Apply habilitado sin cambios")
	}
	if p.power.Text != "Power limit: 123 W (default 145 W)" || !strings.Contains(p.ecoNote.Text, "100-145 W") {
		t.Fatalf("potencia = %q / %q", p.power.Text, p.ecoNote.Text)
	}
	if p.batteryCard.Visible() {
		t.Fatal("tarjeta de batería visible en un equipo sin batería")
	}
	if p.pauseEntry.Disabled() || p.pauseEntry.Text != "blender, obs*" || !p.pauseSave.Disabled() {
		t.Fatalf("lista de pausa: disabled=%v texto=%q", p.pauseEntry.Disabled(), p.pauseEntry.Text)
	}
	if p.pauseBtn.Disabled() || p.resume.Disabled() {
		t.Fatal("botones de pausa deshabilitados con el daemon conectado")
	}
	if p.updated.Text != "Updated 15:04:05" {
		t.Fatalf("hora = %q", p.updated.Text)
	}
}

func TestPanelOnMac(t *testing.T) {
	p, _, _ := newTestPanel(t)
	p.update(macView())

	if p.mode.Text != "Folding (on battery)" {
		t.Fatalf("modo = %q", p.mode.Text)
	}
	if p.ecoBox.Visible() {
		t.Fatal("control de EcoWatts visible sin control de potencia")
	}
	if p.gpuName.Text != "Apple M4" || p.usage.Text != "GPU utilization: 24 % · In-use memory: 1234 MiB" {
		t.Fatalf("gpu = %q / %q", p.gpuName.Text, p.usage.Text)
	}
	for _, fake := range []string{" W", "°C", "VRAM", "Mock"} {
		if strings.Contains(p.usage.Text, fake) || strings.Contains(p.gpuName.Text, fake) {
			t.Fatalf("dato simulado %q en el panel: %q / %q", fake, p.gpuName.Text, p.usage.Text)
		}
	}

	// FAH not running: the header must not claim folding.
	v := macView()
	v.Status.FAH.Reachable = false
	p.update(v)
	if p.mode.Text != "Folding@home Not Reachable" || !strings.Contains(p.fah.Text, "not reachable") {
		t.Fatalf("FAH caído: %q / %q", p.mode.Text, p.fah.Text)
	}
	p.update(macView())
	if !strings.Contains(p.ecoNote.Text, "firmware") || !strings.Contains(p.power.Text, "firmware") {
		t.Fatalf("falta la explicación del firmware: %q / %q", p.ecoNote.Text, p.power.Text)
	}
	if !p.batteryCard.Visible() || p.battery.Text != "53 %, on battery" {
		t.Fatalf("batería: visible=%v texto=%q", p.batteryCard.Visible(), p.battery.Text)
	}
	if !p.pauseEntry.Disabled() || !p.pauseSave.Disabled() || !strings.Contains(p.pauseNote.Text, "Not available") {
		t.Fatalf("lista de pausa editable donde no hay soporte: %q", p.pauseNote.Text)
	}
}

func TestPanelStates(t *testing.T) {
	p, rec, _ := newTestPanel(t)

	v := linuxView()
	v.Status.Mode, v.Status.ManualOverride, v.Status.PausedByUs = gui.ModePausedManual, gui.OverridePause, true
	v.Status.FAH.Paused = true
	p.update(v)
	if !p.badge.Visible() || p.mode.Text != "Paused (Manual)" || p.fah.Text != "Folding@home: paused by EcoFold" {
		t.Fatalf("pausa manual: badge=%v modo=%q fah=%q", p.badge.Visible(), p.mode.Text, p.fah.Text)
	}
	if !p.pauseBtn.Disabled() || p.resume.Disabled() {
		t.Fatal("con pausa manual se esperaba Pause deshabilitado y Resume habilitado")
	}
	test.Tap(p.resume)
	if rec.resumes != 1 {
		t.Fatalf("resume = %d", rec.resumes)
	}

	v = linuxView()
	v.Status.Mode = gui.ModePausedLoad
	v.Status.GameSessions = []string{"steam:1422450"}
	v.Message, v.MessageIsError = "Not resumed: a game or GPU load is active.", true
	p.update(v)
	if !p.sessions.Visible() || p.sessions.Text != "Game sessions: steam:1422450" {
		t.Fatalf("sesiones = %q", p.sessions.Text)
	}
	if p.message.Text != v.Message || p.message.Importance != widget.DangerImportance {
		t.Fatalf("barra de estado = %q (%v)", p.message.Text, p.message.Importance)
	}
	test.Tap(p.pauseBtn)
	if rec.pauses != 1 {
		t.Fatalf("pause = %d", rec.pauses)
	}

	p.update(gui.View{Offline: "Daemon not running", OfflineHint: "Start ecofold-daemon."})
	if p.mode.Text != "Daemon Offline" || !p.offline.Visible() || !strings.Contains(p.offline.Text, "Daemon not running") {
		t.Fatalf("offline: modo=%q aviso=%q", p.mode.Text, p.offline.Text)
	}
	if !p.pauseBtn.Disabled() || !p.resume.Disabled() || p.ecoBox.Visible() || !p.pauseEntry.Disabled() || p.fah.Visible() {
		t.Fatal("controles activos sin daemon")
	}
	if p.updated.Text != "offline" {
		t.Fatalf("hora = %q", p.updated.Text)
	}

	// And back: the panel recovers with the daemon.
	p.update(linuxView())
	if p.offline.Visible() || p.pauseBtn.Disabled() || !p.ecoBox.Visible() {
		t.Fatal("el panel no se recuperó al volver el daemon")
	}
}

// TestPanelEditsSurviveRefresh: the 2 s refresh must not undo what the user
// is typing or dragging.
func TestPanelEditsSurviveRefresh(t *testing.T) {
	p, rec, _ := newTestPanel(t)
	p.update(linuxView())

	p.ecoSlide.SetValue(110)
	if p.ecoApply.Disabled() || p.ecoValue.Text != "110 W" {
		t.Fatalf("tras mover el slider: apply disabled=%v valor=%q", p.ecoApply.Disabled(), p.ecoValue.Text)
	}
	p.pauseEntry.SetText("blender, obs*, resolve") // as typed: OnChanged fires
	p.update(linuxView())
	if p.ecoSlide.Value != 110 || p.pauseEntry.Text != "blender, obs*, resolve" {
		t.Fatalf("el refresco pisó la edición: %v / %q", p.ecoSlide.Value, p.pauseEntry.Text)
	}

	test.Tap(p.ecoApply)
	test.Tap(p.pauseSave)
	if len(rec.watts) != 1 || rec.watts[0] != 110 || len(rec.lists) != 1 || rec.lists[0] != "blender, obs*, resolve" {
		t.Fatalf("acciones = %v / %q", rec.watts, rec.lists)
	}

	// Once applied, the daemon's value wins again: a refused change snaps
	// back.
	p.update(linuxView())
	if p.ecoSlide.Value != 123 || p.pauseEntry.Text != "blender, obs*" || !p.ecoApply.Disabled() || !p.pauseSave.Disabled() {
		t.Fatalf("tras aplicar: %v / %q", p.ecoSlide.Value, p.pauseEntry.Text)
	}
}
