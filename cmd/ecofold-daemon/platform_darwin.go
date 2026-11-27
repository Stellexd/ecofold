//go:build darwin

package main

import (
	"os"
	"path/filepath"

	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// defaultRuntimeDir is per user, because the macOS daemon runs without root.
func defaultRuntimeDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "ecofold")
	}
	return filepath.Join(os.TempDir(), "ecofold")
}

// pauseListSupported reports whether newCollector honors the pause list. The
// macOS collector lists processes with sysctl kern.proc.all.
const pauseListSupported = true

func newCollector(gpu.Provider) daemon.Collector {
	return detector.NewDarwinCollector()
}

// socketGroupFlag registers nothing: on macOS the daemon runs as the user and
// only that user may connect.
func socketGroupFlag() *string {
	return new(string)
}
