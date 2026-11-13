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

func newCollector(p gpu.Provider) daemon.Collector {
	return detector.NewLinuxCollector(p)
}
