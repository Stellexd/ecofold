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
// macOS collector reads ioreg only and has no per-process view yet.
const pauseListSupported = false

func newCollector(gpu.Provider, []string) daemon.Collector {
	return detector.NewDarwinCollector()
}
