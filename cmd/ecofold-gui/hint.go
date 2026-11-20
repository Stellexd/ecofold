//go:build cgo

package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// hintButton is a button that, while disabled, says why when the mouse is
// over it. Fyne has no tooltips of its own.
type hintButton struct {
	widget.Button
	hint    string
	onHover func(b *hintButton, in bool)
}

func newHintButton(label string, icon fyne.Resource, tapped func()) *hintButton {
	b := &hintButton{}
	b.Text, b.Icon, b.OnTapped = label, icon, tapped
	b.ExtendBaseWidget(b)
	return b
}

// setHint disables the button with hint as the reason, or enables it when
// hint is empty.
func (b *hintButton) setHint(hint string) {
	b.hint = hint
	enable(b, hint == "")
}

func (b *hintButton) MouseIn(e *desktop.MouseEvent) {
	b.Button.MouseIn(e)
	if b.onHover != nil {
		b.onHover(b, true)
	}
}

func (b *hintButton) MouseOut() {
	b.Button.MouseOut()
	if b.onHover != nil {
		b.onHover(b, false)
	}
}

// tipLayer draws the hint of the hovered button above the panel. It lives in
// the window content, not in a canvas overlay: an overlay takes the mouse
// events, so the button would get MouseOut as soon as the tip appeared.
type tipLayer struct {
	layer *fyne.Container
	box   *fyne.Container
	bg    *canvas.Rectangle
	text  *widget.Label
	// over is the button whose hint is shown, nil when hidden.
	over *hintButton
}

func newTipLayer() *tipLayer {
	t := &tipLayer{bg: canvas.NewRectangle(nil), text: widget.NewLabel("")}
	t.bg.CornerRadius = theme.InputRadiusSize()
	t.text.Wrapping = fyne.TextWrapWord
	t.box = container.NewStack(t.bg, t.text)
	t.box.Hide()
	t.layer = container.NewWithoutLayout(t.box)
	return t
}

// hover shows b's hint under it while the mouse is in and b is disabled.
func (t *tipLayer) hover(b *hintButton, in bool) {
	if !in || b.hint == "" || !b.Disabled() {
		if t.over == b || !in {
			t.over = nil
			t.box.Hide()
		}
		return
	}
	t.over = b
	d := fyne.CurrentApp().Driver()
	pos := d.AbsolutePositionForObject(b).Subtract(d.AbsolutePositionForObject(t.layer))
	t.text.SetText(b.hint)
	t.text.Resize(fyne.NewSize(b.Size().Width, t.text.MinSize().Height))
	size := fyne.NewSize(b.Size().Width, t.text.MinSize().Height)
	t.bg.FillColor = theme.Color(theme.ColorNameOverlayBackground)
	t.bg.StrokeColor = theme.Color(theme.ColorNameSeparator)
	t.bg.StrokeWidth = 1
	t.box.Resize(size)
	t.box.Move(pos.AddXY(0, b.Size().Height+theme.Padding()))
	t.box.Show()
	t.box.Refresh()
}

// refresh follows a hint that changed while the mouse stays on its button.
func (t *tipLayer) refresh() {
	if t.over != nil {
		t.hover(t.over, true)
	}
}
