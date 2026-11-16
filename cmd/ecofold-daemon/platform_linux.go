//go:build linux

package main

import (
	"flag"

	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/gpu"
	"github.com/Stellexd/ecofold/internal/ipc"
)

// defaultRuntimeDir is created by systemd (RuntimeDirectory=ecofold) and is
// cleared on reboot.
func defaultRuntimeDir() string {
	return "/run/ecofold"
}

// pauseListSupported reports whether newCollector honors the pause list. The
// daemon hands the list to the collector at startup.
const pauseListSupported = true

func newCollector(p gpu.Provider) daemon.Collector {
	return detector.NewLinuxCollector(p)
}

// socketGroupFlag registers -socket-group: the group whose members may use
// the socket besides root.
func socketGroupFlag() *string {
	return flag.String("socket-group", ipc.DefaultGroup,
		"group allowed to use the socket; if it does not exist the socket is root-only")
}
