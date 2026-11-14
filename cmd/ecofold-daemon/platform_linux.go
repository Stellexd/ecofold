//go:build linux

package main

import (
	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// defaultRuntimeDir is created by systemd (RuntimeDirectory=ecofold) and is
// cleared on reboot.
func defaultRuntimeDir() string {
	return "/run/ecofold"
}

// pauseListSupported reports whether newCollector honors the pause list.
const pauseListSupported = true

func newCollector(p gpu.Provider, pause []string) daemon.Collector {
	c := detector.NewLinuxCollector(p)
	c.SetPauseProcesses(pause)
	return c
}
