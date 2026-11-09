// Package gpu abstracts GPU telemetry and power-limit control across Linux (NVML)
// and macOS (Apple Silicon mock) builds.
package gpu

import (
	"fmt"
	"math"
)

// Telemetry represents a snapshot of GPU metrics using integer units for UI consumption.
type Telemetry struct {
	Name               string
	UUID               string
	TempC              int
	PowerUsageW        int
	PowerLimitW        int
	MinPowerLimitW     int
	MaxPowerLimitW     int
	DefaultPowerLimitW int
	UsedVRAMMiB        int
	TotalVRAMMiB       int
	SupportsPowerLimit bool
}

// ProcessInfo describes a single process running on the GPU.
type ProcessInfo struct {
	PID         uint32
	UsedVRAMMiB uint64
}

// Provider abstracts GPU telemetry and power limit control across platforms.
type Provider interface {
	Init() error
	Shutdown() error
	GetTelemetry() (Telemetry, error)
	SetPowerLimit(watts int) error
	RestoreDefaultPowerLimit() error
	GetGraphicsProcesses() ([]ProcessInfo, error)
}

// validatePowerLimit ensures watts is within the inclusive [lo, hi] range.
func validatePowerLimit(watts, lo, hi int) error {
	if watts < lo {
		return fmt.Errorf("power limit %dW below minimum %dW", watts, lo)
	}
	if watts > hi {
		return fmt.Errorf("power limit %dW above maximum %dW", watts, hi)
	}
	return nil
}

// mwToW converts milliwatts to watts using integer division.
func mwToW(mw uint32) int {
	return int(mw / 1000)
}

// wToMW converts watts to milliwatts. It rejects values that would overflow
// an unsigned 32-bit integer after scaling.
func wToMW(w int) (uint32, error) {
	const maxWatts = math.MaxUint32 / 1000
	if w < 0 {
		return 0, fmt.Errorf("power limit %dW is negative", w)
	}
	if w > maxWatts {
		return 0, fmt.Errorf("power limit %dW exceeds maximum convertible value %dW", w, maxWatts)
	}
	return uint32(w * 1000), nil
}

// bytesToMiB converts bytes to mebibytes using integer division. If the NVML
// unavailable sentinel (math.MaxUint64) is passed, it returns 0.
func bytesToMiB(b uint64) uint64 {
	if b == math.MaxUint64 {
		return 0
	}
	return b / 1024 / 1024
}
