//go:build darwin

package gpu

import (
	"fmt"
	"sync"
)

// MockProvider simulates an NVIDIA GPU on Apple Silicon for development.
type MockProvider struct {
	mu          sync.RWMutex
	initialized bool

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

// Init initializes the mock provider with stable simulated values.
func (p *MockProvider) Init() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.name = "Mock NVIDIA GeForce RTX 5060 (Apple Silicon Dev Mode)"
	p.uuid = "GPU-APPLE-MOCK-5060"
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
	p.initialized = true
	return nil
}

// Shutdown marks the mock provider as uninitialized. Like NVML, every other
// method fails until Init is called again.
func (p *MockProvider) Shutdown() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.initialized = false
	return nil
}

// GetTelemetry returns the current simulated telemetry snapshot.
func (p *MockProvider) GetTelemetry() (Telemetry, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.initialized {
		return Telemetry{}, errNotInitialized()
	}
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

// SetPowerLimit rejects any request because the mock device does not support
// power-limit adjustments on Apple Silicon.
func (p *MockProvider) SetPowerLimit(watts int) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.initialized {
		return errNotInitialized()
	}
	return fmt.Errorf("power limit not supported on this device")
}

// RestoreDefaultPowerLimit rejects the request because the mock device does not
// support power-limit adjustments on Apple Silicon.
func (p *MockProvider) RestoreDefaultPowerLimit() error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.initialized {
		return errNotInitialized()
	}
	return fmt.Errorf("power limit not supported on this device")
}

// GetGraphicsProcesses returns the simulated list of graphics processes.
func (p *MockProvider) GetGraphicsProcesses() ([]ProcessInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.initialized {
		return nil, errNotInitialized()
	}
	out := make([]ProcessInfo, len(p.processes))
	copy(out, p.processes)
	return out, nil
}

// NewProvider returns the macOS mock Provider implementation.
func NewProvider() Provider {
	return &MockProvider{}
}
