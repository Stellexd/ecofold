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
	pauseBtn *widget.Button
	resume   *widget.Button

	gpuName  *widget.Label
	power    *widget.Label
	usage    *widget.Label
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
	p.pauseBtn = widget.NewButtonWithIcon("Pause Folding", theme.MediaPauseIcon(), func() { p.act.pause() })
	p.resume = widget.NewButtonWithIcon("Resume Folding", theme.MediaPlayIcon(), func() { p.act.resume() })

	dotBox := container.NewGridWrap(fyne.NewSize(18, 18), p.dot)
	header := container.NewVBox(
		container.NewBorder(nil, nil, container.NewCenter(dotBox), p.badge, p.mode),
		p.fah,
		p.sessions,
		p.offline,
		container.NewGridWithColumns(2, p.pauseBtn, p.resume),
	)

	p.gpuName = widget.NewLabel("")
	p.power = widget.NewLabel("")
	p.usage = widget.NewLabel("")
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
	gpuBox := container.NewVBox(p.gpuName, p.power, p.usage, p.ecoBox, p.ecoNote)
	gpuCard := widget.NewCard("GPU and power", "", gpuBox)

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

	p.message = widget.NewLabel("")
	p.message.Wrapping = fyne.TextWrapWord
	p.updated = widget.NewLabel("")
	statusBar := container.NewBorder(widget.NewSeparator(), nil, nil, p.updated, p.message)

	body := container.NewVBox(header, gpuCard, p.batteryCard, pauseCard, layout.NewSpacer())
	p.root = container.NewBorder(nil, statusBar, nil, nil, container.NewPadded(container.NewVScroll(body)))
	p.boxes = []*fyne.Container{ecoRight, p.ecoBox, gpuBox, pauseBox, header, body, statusBar}
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
	enable(p.pauseBtn, on && s.ManualOverride != gui.OverridePause)
	enable(p.resume, on)

	// GPU and power.
	p.gpuName.SetText(orDash(s.GPU.Name))
	p.power.SetText(v.PowerLabel())
	p.usage.SetText(v.UsageLabel())
	show(p.gpuName, on)
	show(p.power, on)
	show(p.usage, on)
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
