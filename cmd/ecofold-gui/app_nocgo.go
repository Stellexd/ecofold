//go:build !cgo

package main

import "log/slog"

// runUI needs Fyne's OpenGL driver, which is cgo. A build without cgo (the
// cross-compile check in the Makefile) still compiles, and says why it
// cannot show anything.
func runUI(logger *slog.Logger, _ options) int {
	logger.Error("ecofold-gui was built without cgo and has no graphical interface; rebuild with CGO_ENABLED=1")
	return 1
}
