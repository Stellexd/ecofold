//go:build cgo

package main

import (
	"fmt"
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Stellexd/ecofold/internal/gui"
)

// actions are what the panel asks for. Each must return at once: the caller
// runs the request off the UI thread.
type actions struct {
	pause        func()
	resume       func()
	setEcoWatts  func(watts int)
	setPauseList func(text string)
	openURL      func(url string)
	// refresh asks the daemon for its status now; notify shows a message
	// in the status bar. detectFAH looks for a Folding@home install on
	// this system, without running anything.
	refresh   func()
	notify    func(msg string)
	detectFAH func() gui.FAHInstallGuide
}

// Compute engines offered by the selector. Only Folding@home is wired; the
// BOINC projects are announced for v0.2.
const engineFAH = "Folding@home (Molecular Dynamics)"

var boincEngines = []struct{ option, name string }{
	{"BOINC — Einstein@Home (Astrophysics — Coming in v0.2)", "Einstein@Home"},
	{"BOINC — Rosetta@home (Protein Design — Coming in v0.2)", "Rosetta@home"},
	{"BOINC — World Community Grid (Medical — Coming in v0.2)", "World Community Grid"},
}

// Preference keys of the leaderboard opt-in. They are only stored locally:
// nothing is sent until ecofold-web exists.
const (
	prefShareStats  = "leaderboard.share_stats"
	prefPublicAlias = "leaderboard.public_alias"
	prefClientUUID  = "client_uuid"
)

// metricTile is one cell of the GPU grid: a caption over a figure.
type metricTile struct {
	caption *widget.Label
	value   *widget.Label
}

func newMetricTile() (metricTile, fyne.CanvasObject) {
	t := metricTile{
		caption: widget.NewLabel(""),
		value:   widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	}
	t.caption.SizeName = theme.SizeNameCaptionText
	// Three columns leave no room for a larger figure ("Firmware Managed").
	t.value.Truncation = fyne.TextTruncateEllipsis
	return t, container.NewVBox(t.caption, t.value)
}

// panel is the control window's content. It only uses widgets, so it is
// tested with Fyne's software driver, without a display. Every method must
// run on the UI thread.
type panel struct {
	act actions

	dot      *canvas.Circle
	mode     *widget.Label
	fah      *widget.Label
	sessions *widget.Label
	badge    *widget.Label
	offline  *widget.Label
	pauseBtn *hintButton
	resume   *hintButton
	tips     *tipLayer

	// The Folding@home setup card, shown while FAH does not answer. guide
	// is detected when the panel is built and again on Check Again.
	setupCard   *widget.Card
	setupDesc   *widget.Label
	setupCmdBox *fyne.Container
	setupLabel  *widget.Label
	setupCmd    *widget.Label
	guide       gui.FAHInstallGuide

	engine      *widget.Select
	engineNote  *widget.Label
	projBox     *fyne.Container
	projTitle   *widget.Label
	projCause   *widget.Label
	projOrg     *widget.Label
	projSummary *widget.Label
	projBtn     *widget.Button
	projNone    *widget.Label
	// projectID is the project the details button opens.
	projectID int

	gpuName  *widget.Label
	metrics  [3]metricTile
	grid     *fyne.Container
	ecoBox   *fyne.Container
	ecoSlide *widget.Slider
	ecoValue *widget.Label
	ecoApply *widget.Button
	ecoNote  *widget.Label

	batteryCard *widget.Card
	battery     *widget.Label

	pauseEntry *widget.Entry
	pauseSave  *widget.Button
	pauseNote  *widget.Label
	matches    *widget.Label

	compute    *widget.Label
	shareCheck *widget.Check
	aliasEntry *widget.Entry

	message *widget.Label
	updated *widget.Label

	root fyne.CanvasObject
	// boxes are the containers whose children change size or visibility,
	// inner first: neither lays the parent out again by itself.
	boxes []*fyne.Container

	// ecoDirty and pauseDirty are set while the user has an edit that is not
	// applied yet, so a status refresh does not overwrite it. syncing guards
	// the change callbacks while the panel itself writes a value.
	ecoDirty   bool
	pauseDirty bool
	syncing    bool
}

func newPanel(act actions) *panel {
	p := &panel{act: act}

	p.dot = canvas.NewCircle(gui.ToneGray.Color())
	p.mode = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.mode.SizeName = theme.SizeNameSubHeadingText
	p.fah = widget.NewLabel("")
	p.sessions = widget.NewLabel("")
	p.sessions.Wrapping = fyne.TextWrapWord
	p.badge = widget.NewLabelWithStyle("MANUAL PAUSE", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true})
	p.badge.Importance = widget.WarningImportance
	p.offline = widget.NewLabel("")
	p.offline.Wrapping = fyne.TextWrapWord
	p.tips = newTipLayer()
	p.pauseBtn = newHintButton("Pause Folding", theme.MediaPauseIcon(), func() { p.act.pause() })
	p.resume = newHintButton("Resume Folding", theme.MediaPlayIcon(), func() { p.act.resume() })
	p.pauseBtn.onHover, p.resume.onHover = p.tips.hover, p.tips.hover

	dotBox := container.NewGridWrap(fyne.NewSize(18, 18), p.dot)
	header := container.NewVBox(
		container.NewBorder(nil, nil, container.NewCenter(dotBox), p.badge, p.mode),
		p.fah,
		p.sessions,
		p.offline,
		container.NewGridWithColumns(2, p.pauseBtn, p.resume),
	)

	p.setupDesc = widget.NewLabel("")
	p.setupDesc.Wrapping = fyne.TextWrapWord
	download := widget.NewButtonWithIcon("Download Folding@home", theme.DownloadIcon(), func() {
		p.act.openURL(p.guide.DownloadURL)
	})
	download.Importance = widget.HighImportance
	p.setupLabel = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.setupCmd = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	p.setupCmd.Wrapping = fyne.TextWrapBreak
	p.setupCmd.Selectable = true
	copyCmd := widget.NewButtonWithIcon("Copy Command", theme.ContentCopyIcon(), func() {
		fyne.CurrentApp().Clipboard().SetContent(p.guide.InstallCommand)
		p.act.notify("Install command copied to clipboard")
	})
	p.setupCmdBox = container.NewVBox(p.setupLabel,
		container.NewBorder(nil, nil, nil, copyCmd, p.setupCmd))
	checkAgain := widget.NewButtonWithIcon("Check Again", theme.ViewRefreshIcon(), func() {
		p.setGuide(p.act.detectFAH())
		p.act.refresh()
	})
	setupBox := container.NewVBox(p.setupDesc, p.setupCmdBox,
		container.NewHBox(download, checkAgain))
	p.setupCard = widget.NewCard("Folding@home Setup", "", setupBox)
	p.setupCard.Hide()
	p.setGuide(p.act.detectFAH())

	options := []string{engineFAH}
	for _, e := range boincEngines {
		options = append(options, e.option)
	}
	p.engineNote = widget.NewLabel("")
	p.engineNote.Wrapping = fyne.TextWrapWord
	p.engineNote.Importance = widget.WarningImportance
	p.engineNote.Hide()
	p.engine = widget.NewSelect(options, nil)
	p.engine.SetSelected(engineFAH)
	p.engine.OnChanged = func(option string) {
		for _, e := range boincEngines {
			if e.option != option {
				continue
			}
			p.engineNote.SetText(e.name + " support arrives in v0.2, with the BOINC RPC connector. " +
				"Until then Folding@home stays the active engine.")
			p.engineNote.Show()
			// Folding@home is what runs, so it is what stays selected.
			p.engine.SetSelected(engineFAH)
			p.refreshLayout()
		}
	}
	p.projTitle = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.projTitle.SizeName = theme.SizeNameSubHeadingText
	p.projCause = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.projCause.Importance = widget.SuccessImportance
	p.projCause.Wrapping = fyne.TextWrapWord
	p.projOrg = widget.NewLabel("")
	p.projOrg.Wrapping = fyne.TextWrapWord
	p.projSummary = widget.NewLabel("")
	p.projSummary.Wrapping = fyne.TextWrapWord
	p.projSummary.TextStyle = fyne.TextStyle{Italic: true}
	p.projBtn = widget.NewButtonWithIcon("View Project Details…", theme.SearchIcon(), func() {
		if p.projectID > 0 {
			p.act.openURL(gui.ProjectURL(p.projectID))
		}
	})
	p.projNone = widget.NewLabel("")
	p.projBox = container.NewVBox(p.projTitle, p.projCause, p.projOrg, p.projSummary,
		container.NewHBox(p.projBtn))
	impactBox := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Compute Engine"), nil, p.engine),
		p.engineNote,
		widget.NewSeparator(),
		p.projBox,
		p.projNone,
	)
	impactCard := widget.NewCard("Scientific Impact & Compute Engine", "", impactBox)

	p.gpuName = widget.NewLabel("")
	p.grid = container.NewGridWithColumns(3)
	for i := range p.metrics {
		tile, box := newMetricTile()
		p.metrics[i] = tile
		p.grid.Add(box)
	}
	p.ecoSlide = widget.NewSlider(0, 1)
	p.ecoSlide.Step = 1
	p.ecoValue = widget.NewLabel("")
	p.ecoApply = widget.NewButton("Apply", func() {
		p.ecoDirty = false
		p.ecoApply.Disable()
		p.act.setEcoWatts(int(p.ecoSlide.Value))
	})
	p.ecoApply.Importance = widget.HighImportance
	p.ecoSlide.OnChanged = func(v float64) {
		p.ecoValue.SetText(fmt.Sprintf("%d W", int(v)))
		if !p.syncing {
			p.ecoDirty = true
			p.ecoApply.Enable()
		}
	}
	ecoRight := container.NewHBox(p.ecoValue, p.ecoApply)
	p.ecoBox = container.NewBorder(nil, nil, widget.NewLabel("Eco limit while folding"), ecoRight, p.ecoSlide)
	p.ecoNote = widget.NewLabel("")
	p.ecoNote.Wrapping = fyne.TextWrapWord
	gpuBox := container.NewVBox(p.gpuName, p.grid, p.ecoBox, p.ecoNote)
	gpuCard := widget.NewCard("GPU and Power", "", gpuBox)

	p.battery = widget.NewLabel("")
	p.batteryCard = widget.NewCard("Battery", "", p.battery)

	p.pauseEntry = widget.NewEntry()
	p.pauseEntry.SetPlaceHolder("blender, obs*")
	p.pauseSave = widget.NewButton("Save", func() {
		p.pauseDirty = false
		p.pauseSave.Disable()
		p.act.setPauseList(p.pauseEntry.Text)
	})
	p.pauseEntry.OnChanged = func(string) {
		if !p.syncing {
			p.pauseDirty = true
			p.pauseSave.Enable()
		}
	}
	p.pauseEntry.OnSubmitted = func(string) {
		if !p.pauseSave.Disabled() {
			p.pauseSave.OnTapped()
		}
	}
	p.pauseNote = widget.NewLabel("")
	p.pauseNote.Wrapping = fyne.TextWrapWord
	p.matches = widget.NewLabel("")
	pauseBox := container.NewVBox(container.NewBorder(nil, nil, nil, p.pauseSave, p.pauseEntry), p.matches, p.pauseNote)
	pauseCard := widget.NewCard("Pause processes", "Apps that stop folding while they run", pauseBox)

	// The opt-in is off until the user ticks it (Zero-Login) and lives only
	// in this user's preferences.
	prefs := fyne.CurrentApp().Preferences()
	if prefs.String(prefClientUUID) == "" {
		prefs.SetString(prefClientUUID, gui.NewClientUUID())
	}
	p.compute = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.aliasEntry = widget.NewEntry()
	p.aliasEntry.SetPlaceHolder("Public Alias (optional)")
	p.aliasEntry.SetText(prefs.String(prefPublicAlias))
	p.aliasEntry.OnChanged = func(alias string) { prefs.SetString(prefPublicAlias, strings.TrimSpace(alias)) }
	p.shareCheck = widget.NewCheck("Share anonymous compute stats on the Global Leaderboard", func(on bool) {
		prefs.SetBool(prefShareStats, on)
		enable(p.aliasEntry, on)
	})
	p.shareCheck.SetChecked(prefs.Bool(prefShareStats))
	enable(p.aliasEntry, p.shareCheck.Checked)
	boardNote := widget.NewLabel("100% optional. Helps track total community hours and work units donated. " +
		"Nothing is sent yet: your choice is saved for when the leaderboard opens.")
	boardNote.Wrapping = fyne.TextWrapWord
	boardNote.SizeName = theme.SizeNameCaptionText
	boardCard := widget.NewCard("Community Leaderboard (Opt-in)", "",
		container.NewVBox(p.compute, p.shareCheck, p.aliasEntry, boardNote))

	p.message = widget.NewLabel("")
	p.message.Wrapping = fyne.TextWrapWord
	p.updated = widget.NewLabel("")
	statusBar := container.NewBorder(widget.NewSeparator(), nil, nil, p.updated, p.message)

	body := container.NewVBox(header, p.setupCard, impactCard, boardCard, gpuCard, p.batteryCard, pauseCard, layout.NewSpacer())
	p.root = container.NewStack(
		container.NewBorder(nil, statusBar, nil, nil, container.NewPadded(container.NewVScroll(body))),
		p.tips.layer)
	p.boxes = []*fyne.Container{p.setupCmdBox, setupBox, p.projBox, impactBox, ecoRight, p.ecoBox, p.grid, gpuBox, pauseBox, header, body, statusBar}
	return p
}

// update renders v.
func (p *panel) update(v gui.View) {
	p.syncing = true
	defer func() { p.syncing = false }()

	s := v.Status
	p.dot.FillColor = color.Color(v.Tone().Color())
	p.dot.Refresh()
	p.mode.SetText(v.ModeLabel())

	on := v.Connected
	show(p.offline, !on)
	show(p.fah, on)
	show(p.badge, on && s.ManualOverride == gui.OverridePause)
	if !on {
		p.offline.SetText(strings.TrimSpace(v.Offline + ". " + v.OfflineHint))
	} else {
		p.fah.SetText(v.FAHLabel())
	}
	show(p.sessions, on && len(s.GameSessions) > 0)
	p.sessions.SetText("Game sessions: " + strings.Join(s.GameSessions, ", "))
	p.pauseBtn.setHint(v.PauseHint())
	p.resume.setHint(v.ResumeHint())
	p.tips.refresh()

	// Folding@home setup, until the daemon reaches it.
	show(p.setupCard, v.NeedsFAHSetup())

	// Scientific impact. Without metadata (not fetched yet, or no network)
	// the card still names the project and links to it.
	p.projectID = s.FAH.ActiveProjectID
	hasProject := on && p.projectID > 0
	show(p.projBox, hasProject)
	show(p.projNone, !hasProject)
	if on {
		p.projNone.SetText("No active project right now.")
	} else {
		p.projNone.SetText("Not connected to the daemon.")
	}
	if hasProject {
		org := v.Project.Institution
		if org != "" && v.Project.Manager != "" {
			org += " · " + v.Project.Manager
		}
		p.projTitle.SetText(gui.ProjectTitle(p.projectID))
		p.projCause.SetText(v.CauseLabel())
		p.projOrg.SetText(org)
		p.projSummary.SetText(v.Project.Summary)
		show(p.projCause, p.projCause.Text != "")
		show(p.projOrg, org != "")
		show(p.projSummary, v.Project.Summary != "")
	}

	// GPU and power.
	p.gpuName.SetText(orDash(s.GPU.Name))
	for i, m := range v.Metrics() {
		p.metrics[i].caption.SetText(m.Label)
		p.metrics[i].value.SetText(m.Value)
	}
	show(p.gpuName, on)
	show(p.grid, on)
	canCap := on && s.PowerCapable && s.MaxPowerLimitW > s.MinPowerLimitW
	show(p.ecoBox, canCap)
	switch {
	case !on:
		p.ecoNote.SetText("Not connected to the daemon.")
	case !s.PowerCapable:
		p.ecoNote.SetText("This system manages GPU power in firmware, so there is no power limit to set. " +
			"EcoFold only pauses and resumes folding here.")
	case !canCap:
		p.ecoNote.SetText(fmt.Sprintf("This GPU only accepts %d W.", s.MaxPowerLimitW))
	case !s.PowerEnabled:
		p.ecoNote.SetText("The daemon is not applying power limits right now (no permission or degraded mode).")
	default:
		p.ecoNote.SetText(fmt.Sprintf("Allowed range: %d-%d W.", s.MinPowerLimitW, s.MaxPowerLimitW))
	}
	if canCap {
		p.ecoSlide.Min, p.ecoSlide.Max = float64(s.MinPowerLimitW), float64(s.MaxPowerLimitW)
		if !p.ecoDirty {
			p.ecoSlide.SetValue(float64(s.EcoWatts))
			p.ecoValue.SetText(fmt.Sprintf("%d W", s.EcoWatts))
			p.ecoApply.Disable()
		}
		p.ecoSlide.Refresh()
	}

	// Leaderboard.
	p.compute.SetText(v.ComputeTimeLabel())

	// Battery.
	show(p.batteryCard, on && s.Battery.Present)
	p.battery.SetText(v.BatteryLabel())

	// Pause list.
	canList := on && s.PauseListSupported
	enable(p.pauseEntry, canList)
	if !canList {
		p.pauseDirty = false
		p.pauseSave.Disable()
	}
	if !p.pauseDirty {
		p.pauseEntry.SetText(v.PauseListText())
		p.pauseSave.Disable()
	}
	switch {
	case !on:
		p.pauseNote.SetText("Not connected to the daemon.")
	case !s.PauseListSupported:
		p.pauseNote.SetText("Not available on this system: the daemon cannot see individual processes here.")
	default:
		p.pauseNote.SetText("Process names separated by commas. A trailing * matches a prefix, for example obs*.")
	}
	show(p.matches, canList && len(s.PauseMatches) > 0)
	p.matches.SetText("Running now: " + strings.Join(s.PauseMatches, ", "))

	// Status bar.
	p.message.SetText(v.Message)
	if v.MessageIsError {
		p.message.Importance = widget.DangerImportance
	} else {
		p.message.Importance = widget.MediumImportance
	}
	p.message.Refresh()
	switch {
	case !on:
		p.updated.SetText("offline")
	case s.UpdatedAt.IsZero():
		p.updated.SetText("")
	default:
		p.updated.SetText("Updated " + s.UpdatedAt.Local().Format("15:04:05"))
	}

	p.refreshLayout()
}

// setGuide shows g in the setup card.
func (p *panel) setGuide(g gui.FAHInstallGuide) {
	p.guide = g
	p.setupCard.SetSubTitle(g.Title)
	p.setupDesc.SetText(g.Description)
	p.setupLabel.SetText(g.CommandLabel)
	p.setupCmd.SetText(g.InstallCommand)
	show(p.setupCmdBox, g.InstallCommand != "")
	p.refreshLayout()
}

// refreshLayout lays the containers out again after their children changed
// size or visibility.
func (p *panel) refreshLayout() {
	for _, b := range p.boxes {
		b.Refresh()
	}
}

func show(o fyne.CanvasObject, visible bool) {
	if visible {
		o.Show()
	} else {
		o.Hide()
	}
}

func enable(w fyne.Disableable, on bool) {
	if on {
		w.Enable()
	} else {
		w.Disable()
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
