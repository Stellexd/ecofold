//go:build linux && !cgo

package gpu

import (
	"fmt"
	"sync"
)

// stubProvider is a compile-time placeholder for Linux builds without CGO
// (for example, cross-compilation from macOS). It mirrors the mock provider
// contract so the package always builds, but it does not drive real hardware.
type stubProvider struct {
	mu sync.RWMutex

	name               string
	uuid               string
	tempC              int
	powerUsageW        int
	powerLimitW        int
	minPowerLimitW     int
	maxPowerLimitW     int
	defaultPowerLimitW int
	usedVRAMMiB        int
	totalVRAMMiB       int
	supportsPowerLimit bool

	processes []ProcessInfo
}

// Init initializes the stub provider with stable simulated values.
func (p *stubProvider) Init() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.name = "Stub NVIDIA GeForce RTX 5060 (Linux cross-compile mode)"
	p.uuid = "GPU-LINUX-STUB-5060"
	p.tempC = 42
	p.minPowerLimitW = 123
	p.maxPowerLimitW = 145
	p.defaultPowerLimitW = 145
	p.powerLimitW = p.defaultPowerLimitW
	p.powerUsageW = 75
	p.totalVRAMMiB = 8192
	p.usedVRAMMiB = 512
	p.supportsPowerLimit = false
	p.processes = []ProcessInfo{}
	return nil
}

// Shutdown is a no-op for the stub provider.
func (p *stubProvider) Shutdown() error {
	return nil
}

// GetTelemetry returns the current simulated telemetry snapshot.
func (p *stubProvider) GetTelemetry() (Telemetry, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return Telemetry{
		Name:               p.name,
		UUID:               p.uuid,
		TempC:              p.tempC,
		PowerUsageW:        p.powerUsageW,
		PowerLimitW:        p.powerLimitW,
		MinPowerLimitW:     p.minPowerLimitW,
		MaxPowerLimitW:     p.maxPowerLimitW,
		DefaultPowerLimitW: p.defaultPowerLimitW,
		UsedVRAMMiB:        p.usedVRAMMiB,
		TotalVRAMMiB:       p.totalVRAMMiB,
		SupportsPowerLimit: p.supportsPowerLimit,
	}, nil
}

// SetPowerLimit rejects any request because the stub device does not support
// power-limit adjustments.
func (p *stubProvider) SetPowerLimit(watts int) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return fmt.Errorf("power limit not supported on this device")
}

// RestoreDefaultPowerLimit rejects the request because the stub device does not
// support power-limit adjustments.
func (p *stubProvider) RestoreDefaultPowerLimit() error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return fmt.Errorf("power limit not supported on this device")
}

// GetGraphicsProcesses returns the simulated list of graphics processes.
func (p *stubProvider) GetGraphicsProcesses() ([]ProcessInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]ProcessInfo, len(p.processes))
	copy(out, p.processes)
	return out, nil
}

// NewProvider returns the Linux stub Provider implementation when CGO is
// unavailable.
func NewProvider() Provider {
	return &stubProvider{}
}
