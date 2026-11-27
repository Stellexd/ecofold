//go:build cgo

package main

import (
	"context"
	"log/slog"
	"net/url"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/driver/desktop"

	"github.com/Stellexd/ecofold/internal/gui"
)

const appID = "io.github.stellexd.ecofold"

// runUI shows the tray icon and the control panel and blocks until the user
// quits. Quitting only closes this client; the daemon keeps running.
func runUI(logger *slog.Logger, opts options) int {
	a := app.NewWithID(appID)
	w := a.NewWindow("EcoFold")
	w.Resize(fyne.NewSize(540, 780))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		ctrl    *gui.Controller
		p       *panel
		tray    *trayMenu
		visible = !opts.hidden
	)
	// Requests run off the UI thread; each is bounded by the controller's
	// timeout.
	run := func(fn func(context.Context)) { go fn(ctx) }

	render := func(v gui.View) {
		if tray != nil {
			tray.update(v)
		}
		// A hidden window is not redrawn: the client stays idle in the tray.
		if visible {
			p.update(v)
		}
	}
	ctrl = gui.New(gui.Config{
		SocketPath: opts.socketPath,
		Poll:       opts.poll,
		OnChange:   func(v gui.View) { fyne.Do(func() { render(v) }) },
		Projects:   gui.NewProjectClient(""),
	})
	p = newPanel(actions{
		pause:        func() { run(ctrl.Pause) },
		resume:       func() { run(ctrl.Resume) },
		setEcoWatts:  func(watts int) { run(func(c context.Context) { ctrl.SetEcoWatts(c, watts) }) },
		setPauseList: func(text string) { run(func(c context.Context) { ctrl.SetPauseProcesses(c, text) }) },
		openURL: func(link string) {
			u, err := url.Parse(link)
			if err == nil {
				err = a.OpenURL(u)
			}
			if err != nil {
				logger.Warn("could not open the browser", "url", link, "err", err)
			}
		},
		refresh:   func() { run(ctrl.Refresh) },
		notify:    ctrl.Notify,
		detectFAH: gui.DetectFAHInstallHere,
	})
	w.SetContent(p.root)

	showPanel := func() {
		visible = true
		p.update(ctrl.View())
		w.Show()
		w.RequestFocus()
	}

	if desk, ok := a.(desktop.App); ok {
		tray = newTrayMenu(desk, func() { run(ctrl.Toggle) }, showPanel)
		// The window's X hides it; the tray keeps the client alive.
		w.SetCloseIntercept(func() {
			visible = false
			w.Hide()
		})
	} else {
		logger.Warn("no system tray on this desktop; closing the window quits")
		visible = true
	}

	p.update(ctrl.View())
	if tray != nil {
		tray.update(ctrl.View())
	}
	go ctrl.Run(ctx)

	if visible {
		w.Show()
	}
	a.Run()
	return 0
}

// trayMenu is the system tray icon and its menu.
type trayMenu struct {
	desk   desktop.App
	menu   *fyne.Menu
	info   *fyne.MenuItem
	toggle *fyne.MenuItem
	tone   gui.Tone
	hasTon bool
}

func newTrayMenu(desk desktop.App, onToggle, onOpen func()) *trayMenu {
	t := &trayMenu{desk: desk}
	t.info = fyne.NewMenuItem("Connecting…", nil)
	t.info.Disabled = true
	t.toggle = fyne.NewMenuItem("Pause Folding", onToggle)
	quit := fyne.NewMenuItem("Quit EcoFold GUI", nil)
	quit.IsQuit = true
	t.menu = fyne.NewMenu("EcoFold",
		t.info,
		fyne.NewMenuItemSeparator(),
		t.toggle,
		fyne.NewMenuItem("Open Control Panel…", onOpen),
		fyne.NewMenuItemSeparator(),
		quit,
	)
	desk.SetSystemTrayMenu(t.menu)
	return t
}

// update refreshes the menu, and the icon when its color changes.
func (t *trayMenu) update(v gui.View) {
	if tone := v.Tone(); !t.hasTon || tone != t.tone {
		t.tone, t.hasTon = tone, true
		t.desk.SetSystemTrayIcon(fyne.NewStaticResource("ecofold-"+tone.Name()+".png", gui.IconPNG(tone)))
	}
	headline, label, off := v.Headline(), v.ToggleLabel(), !v.Connected
	if t.info.Label == headline && t.toggle.Label == label && t.toggle.Disabled == off {
		return
	}
	t.info.Label, t.toggle.Label, t.toggle.Disabled = headline, label, off
	t.menu.Refresh()
}
