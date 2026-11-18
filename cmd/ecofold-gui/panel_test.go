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
	urls            []string
}

func (r *recorder) actions() actions {
	return actions{
		pause:        func() { r.pauses++ },
		resume:       func() { r.resumes++ },
		setEcoWatts:  func(w int) { r.watts = append(r.watts, w) },
		setPauseList: func(s string) { r.lists = append(r.lists, s) },
		openURL:      func(u string) { r.urls = append(r.urls, u) },
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
	w.Resize(fyne.NewSize(540, 780))
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
	s.FoldingSeconds, s.ProjectsContributed = 5025, []int{18264}
	s.PauseListSupported = true
	s.PauseProcesses = []string{"blender", "obs*"}
	s.UpdatedAt = time.Date(2026, 11, 16, 15, 4, 5, 0, time.Local)
	return gui.View{Connected: true, Status: s, Project: gui.Project{
		ID:          18264,
		Cause:       "Alzheimer's Disease",
		Institution: "University of Pennsylvania",
		Manager:     "Justin Miller",
		Summary:     "Alzheimer's disease is a significant cause of death and memory loss.",
	}}
}

// metricTexts flattens the GPU grid into "caption=value" cells.
func metricTexts(p *panel) [3]string {
	var out [3]string
	for i, m := range p.metrics {
		out[i] = m.caption.Text + "=" + m.value.Text
	}
	return out
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
	wantGrid := [3]string{"Power=118 W / 123 W", "Temperature=61 °C", "VRAM=812 / 8151 MiB"}
	if p.gpuName.Text != "NVIDIA GeForce RTX 5060" || metricTexts(p) != wantGrid || !p.grid.Visible() || len(p.grid.Objects) != 3 {
		t.Fatalf("gpu = %q / %v", p.gpuName.Text, metricTexts(p))
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
	if !strings.Contains(p.ecoNote.Text, "100-145 W") {
		t.Fatalf("nota de potencia = %q", p.ecoNote.Text)
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
	wantGrid := [3]string{"GPU Load=24 %", "Unified Memory=1234 MiB", "Power Mode=Firmware Managed"}
	if p.gpuName.Text != "Apple M4" || metricTexts(p) != wantGrid {
		t.Fatalf("gpu = %q / %v", p.gpuName.Text, metricTexts(p))
	}
	shown := p.gpuName.Text + strings.Join(wantGrid[:], "")
	for _, fake := range []string{" W", "°C", "VRAM", "Mock"} {
		if strings.Contains(shown, fake) {
			t.Fatalf("dato simulado %q en el panel: %q", fake, shown)
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
	if !strings.Contains(p.ecoNote.Text, "firmware") {
		t.Fatalf("falta la explicación del firmware: %q", p.ecoNote.Text)
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

	// Folding allowed again, the daemon's pause not lifted yet: no green
	// "Folding" over "paused by EcoFold".
	v.Status.Mode, v.Status.ManualOverride = gui.ModeFoldingEco, "none"
	p.update(v)
	if p.mode.Text != "Resuming Folding…" || p.dot.FillColor != gui.ToneGray.Color() {
		t.Fatalf("reanudando: modo=%q", p.mode.Text)
	}
	v.Status.Mode, v.Status.ManualOverride = gui.ModePausedManual, gui.OverridePause
	p.update(v)
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
	if !p.pauseBtn.Disabled() || !p.resume.Disabled() || p.ecoBox.Visible() || !p.pauseEntry.Disabled() || p.fah.Visible() ||
		p.grid.Visible() || p.projBox.Visible() {
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

// TestPanelImpactCard: the project is named and linked with or without its
// metadata, and the BOINC engines only announce v0.2.
func TestPanelImpactCard(t *testing.T) {
	p, rec, _ := newTestPanel(t)
	p.update(linuxView())

	if !p.projBox.Visible() || p.projNone.Visible() || p.projTitle.Text != "Project #18264" {
		t.Fatalf("proyecto: visible=%v título=%q", p.projBox.Visible(), p.projTitle.Text)
	}
	if p.projCause.Text != "Researching: Alzheimer's Disease" || p.projOrg.Text != "University of Pennsylvania · Justin Miller" ||
		!strings.HasPrefix(p.projSummary.Text, "Alzheimer's disease") {
		t.Fatalf("metadatos = %q / %q / %q", p.projCause.Text, p.projOrg.Text, p.projSummary.Text)
	}
	test.Tap(p.projBtn)
	if len(rec.urls) != 1 || rec.urls[0] != "https://stats.foldingathome.org/project/18264" {
		t.Fatalf("enlaces = %q", rec.urls)
	}

	// No network: only the project number, and the link still works.
	v := linuxView()
	v.Project = gui.Project{}
	p.update(v)
	if !p.projBox.Visible() || p.projTitle.Text != "Project #18264" ||
		p.projCause.Visible() || p.projOrg.Visible() || p.projSummary.Visible() {
		t.Fatalf("sin metadatos: título=%q causa=%v org=%v resumen=%v",
			p.projTitle.Text, p.projCause.Visible(), p.projOrg.Visible(), p.projSummary.Visible())
	}
	if p.offline.Visible() || p.pauseBtn.Disabled() {
		t.Fatal("sin metadatos el panel se mostró desconectado")
	}
	test.Tap(p.projBtn)
	if len(rec.urls) != 2 {
		t.Fatalf("enlaces = %q", rec.urls)
	}

	// No work unit: nothing to link.
	v.Status.FAH.ActiveProjectID = 0
	p.update(v)
	if p.projBox.Visible() || !p.projNone.Visible() || p.projNone.Text != "No active project right now." {
		t.Fatalf("sin proyecto: caja=%v nota=%q", p.projBox.Visible(), p.projNone.Text)
	}

	// Compute engine: four options, Folding@home active.
	if len(p.engine.Options) != 4 || p.engine.Selected != engineFAH || p.engineNote.Visible() {
		t.Fatalf("motor: %d opciones, %q, nota=%v", len(p.engine.Options), p.engine.Selected, p.engineNote.Visible())
	}
	p.engine.SetSelected("BOINC — Einstein@Home (Astrophysics — Coming in v0.2)")
	if p.engine.Selected != engineFAH {
		t.Fatalf("motor seleccionado = %q", p.engine.Selected)
	}
	if !p.engineNote.Visible() || !strings.Contains(p.engineNote.Text, "Einstein@Home") ||
		!strings.Contains(p.engineNote.Text, "v0.2") || !strings.Contains(p.engineNote.Text, "BOINC RPC") {
		t.Fatalf("nota de BOINC = %q (visible=%v)", p.engineNote.Text, p.engineNote.Visible())
	}
	// A status refresh keeps the note and the engine.
	p.update(linuxView())
	if !p.engineNote.Visible() || p.engine.Selected != engineFAH || !p.projBox.Visible() {
		t.Fatal("el refresco cambió el selector de motor")
	}
}

// TestPanelLeaderboardOptIn: sharing is off by default, the alias follows
// the check and both survive a restart through the app's preferences.
func TestPanelLeaderboardOptIn(t *testing.T) {
	p, _, _ := newTestPanel(t)
	prefs := fyne.CurrentApp().Preferences()

	if p.shareCheck.Checked || !p.aliasEntry.Disabled() || prefs.Bool(prefShareStats) {
		t.Fatal("el ranking no está desactivado por defecto")
	}
	id := prefs.String(prefClientUUID)
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("client_uuid = %q", id)
	}
	p.update(linuxView())
	if p.compute.Text != "Session compute donated: 1h 23m · Projects: 1" {
		t.Fatalf("tiempo = %q", p.compute.Text)
	}
	if p.shareCheck.Text != "Share anonymous compute stats on the Global Leaderboard" ||
		p.aliasEntry.PlaceHolder != "Public Alias (optional)" {
		t.Fatalf("textos = %q / %q", p.shareCheck.Text, p.aliasEntry.PlaceHolder)
	}

	p.shareCheck.SetChecked(true)
	if p.aliasEntry.Disabled() || !prefs.Bool(prefShareStats) {
		t.Fatal("marcar el check no habilitó el alias o no se guardó")
	}
	p.aliasEntry.SetText("  stellex ")
	if got := prefs.String(prefPublicAlias); got != "stellex" {
		t.Fatalf("alias guardado = %q", got)
	}
	// A status refresh does not touch them.
	p.update(linuxView())
	if !p.shareCheck.Checked || p.aliasEntry.Text != "  stellex " {
		t.Fatalf("tras el refresco: check=%v alias=%q", p.shareCheck.Checked, p.aliasEntry.Text)
	}

	// A new panel on the same app: the choice is restored.
	again := newPanel((&recorder{}).actions())
	if got := prefs.String(prefClientUUID); got != id {
		t.Fatalf("client_uuid cambió: %q -> %q", id, got)
	}
	if !again.shareCheck.Checked || again.aliasEntry.Disabled() || again.aliasEntry.Text != "stellex" {
		t.Fatalf("al reabrir: check=%v alias=%q", again.shareCheck.Checked, again.aliasEntry.Text)
	}

	// Opting out keeps the alias but locks it.
	again.shareCheck.SetChecked(false)
	if prefs.Bool(prefShareStats) || !again.aliasEntry.Disabled() || prefs.String(prefPublicAlias) != "stellex" {
		t.Fatal("desmarcar no desactivó el ranking")
	}
}
