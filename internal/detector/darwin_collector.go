//go:build darwin

package detector

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
)

// DefaultBatteryRefreshTicks is how many Collect calls reuse a cached
// AppleSmartBattery reading. At the daemon's 2 s tick it refreshes the battery
// every 30 s; the GPU is read on every call because game detection needs it.
const DefaultBatteryRefreshTicks = 15

// ErrBatteryVanished is returned by DarwinCollector.Collect when
// AppleSmartBattery reports no battery after an earlier read found one. A
// MacBook does not lose its battery at runtime, so the empty result is a
// transient ioreg glitch; reporting it as a desktop Mac on AC would fold at
// full power on battery (R4).
var ErrBatteryVanished = errors.New("detector: battery vanished after it was seen")

// thermalStaleTicks is how many consecutive failed thermal reads reuse the
// last level read before Collect reports 0 (unknown). It bridges a glitch
// without keeping a stale "serious" forever.
const thermalStaleTicks = 15

// hardwareAttempts is how many Collect calls try to read the hardware model
// before settling for the fallback (unknown model, runtime.NumCPU cores).
const hardwareAttempts = 3

// DarwinCollector builds SystemSnapshot values on macOS from ioreg output.
//
// It reads IOAccelerator and the thermal pressure level on every Collect and
// AppleSmartBattery every DefaultBatteryRefreshTicks calls, so ioreg runs at
// most twice per daemon tick and usually once. The hardware model is read
// with sysctl once (up to hardwareAttempts tries) and classifies the Mac
// (MacProfile, RecommendedCPUs).
//
// DarwinCollector is not safe for concurrent use by multiple goroutines; each
// daemon tick creates one snapshot at a time.
//
// IOAccelerator is read with its user clients (FetchIOAcceleratorClientsContext)
// so the same ioreg run also gives each app's GPU time (GPUApps). If that
// reading fails or cannot be parsed, the tick falls back to the plain
// IOAccelerator reading; after clientAttempts failures in a row the collector
// keeps to the plain reading.
//
// The pause list (SetPauseProcesses) lists processes with sysctl
// kern.proc.all, once per Collect and only while the list is not empty.
type DarwinCollector struct {
	// fetch is the ioreg seam. Production uses FetchIORegContext.
	fetch func(ctx context.Context, class string) ([]byte, error)
	// fetchClients is the seam for IOAccelerator with its user clients.
	// Production uses FetchIOAcceleratorClientsContext; nil reads only
	// IOAccelerator and reports no GPUApps.
	fetchClients func(ctx context.Context) ([]byte, error)
	// listProcs and execName are the process seams. Production uses
	// listDarwinProcs and darwinExecName.
	listProcs func() ([]procEntry, error)
	execName  func(pid int32) (string, error)
	// now is the clock seam for GPUApps.
	now func() time.Time
	// thermal is the thermal pressure seam. Production uses
	// FetchThermalPressureContext; nil reports 0.
	thermal func(ctx context.Context) (int, error)
	// hardware is the hardware model seam. Production uses
	// FetchHardwareInfoContext; nil means unknown.
	hardware func(ctx context.Context) (HardwareInfo, error)

	batteryEvery int
	calls        int
	batt         BatteryMetrics
	battValid    bool
	// seenBattery latches once any read reports HasBattery=true.
	seenBattery bool

	// thermalLevel is the last level read; thermalFails counts the failed
	// reads since.
	thermalLevel int
	thermalFails int

	// hw is the hardware model once read; hwFails counts failed reads, up to
	// hardwareAttempts.
	hw      HardwareInfo
	hwKnown bool
	hwFails int

	// pause is the user's pause list (see ParsePauseProcesses).
	pause []string

	// clientFails counts consecutive failed client readings; clientsOff
	// stops trying them.
	clientFails int
	clientsOff  bool
	// gpuPrev is the GPU time of each user client at gpuPrevAt.
	gpuPrev   map[uint64]gpuSample
	gpuPrevAt time.Time
}

// clientAttempts is how many client readings in a row may fail before the
// collector keeps to the plain IOAccelerator reading.
const clientAttempts = 3

// NewDarwinCollector returns a collector that runs /usr/sbin/ioreg.
func NewDarwinCollector() *DarwinCollector {
	return &DarwinCollector{
		fetch:        FetchIORegContext,
		thermal:      FetchThermalPressureContext,
		hardware:     FetchHardwareInfoContext,
		fetchClients: FetchIOAcceleratorClientsContext,
		listProcs:    listDarwinProcs,
		execName:     darwinExecName,
		batteryEvery: DefaultBatteryRefreshTicks,
	}
}

// Collect returns a snapshot with the current GPU metrics and the cached (or
// freshly read) battery metrics.
//
// Any fetch or parse error is returned instead of an empty snapshot: the
// caller must keep its previous decision rather than feed "no load" to the
// evaluator. A failed battery refresh is retried on the next call. An empty
// AppleSmartBattery result after a battery was seen returns
// ErrBatteryVanished.
//
// A failed thermal read never fails Collect: the last level is reused for up
// to thermalStaleTicks calls, then 0 (unknown) is reported.
func (c *DarwinCollector) Collect(ctx context.Context) (SystemSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return SystemSnapshot{}, err
	}

	every := c.batteryEvery
	if every <= 0 {
		every = DefaultBatteryRefreshTicks
	}
	if !c.battValid || c.calls%every == 0 {
		raw, err := c.fetch(ctx, "AppleSmartBattery")
		if err != nil {
			c.battValid = false
			return SystemSnapshot{}, fmt.Errorf("detector: battery: %w", err)
		}
		batt, err := ParseSmartBatteryPlist(raw)
		if err != nil {
			c.battValid = false
			return SystemSnapshot{}, fmt.Errorf("detector: battery: %w", err)
		}
		if !batt.HasBattery && c.seenBattery {
			c.battValid = false
			return SystemSnapshot{}, ErrBatteryVanished
		}
		c.seenBattery = c.seenBattery || batt.HasBattery
		c.batt, c.battValid = batt, true
		c.calls = 0
	}

	gpu, apps, err := c.readGPU(ctx)
	if err != nil {
		return SystemSnapshot{}, err
	}

	c.calls++
	snap := snapshotFromMetrics(c.batt, gpu)
	snap.GPUApps = apps
	snap.UserPauseMatches = c.userPauseMatches(ctx)
	snap.ThermalLevel = c.readThermal(ctx)
	hw := c.readHardware(ctx)
	snap.MacProfile = ClassifyMacProfile(hw.Model, snap.HasBattery)
	snap.RecommendedCPUs = RecommendedCPUs(snap.MacProfile, hw, snap.OnAC, snap.ThermalLevel)
	return snap, nil
}

// readGPU reads IOAccelerator, with its user clients when it can. The global
// metrics come from the same node either way. Only a failure of the plain
// reading fails Collect.
func (c *DarwinCollector) readGPU(ctx context.Context) (GPUMetrics, []GPUAppUsage, error) {
	if c.fetchClients != nil && !c.clientsOff {
		if raw, err := c.fetchClients(ctx); err == nil {
			if gpu, err := ParseIOAcceleratorPlist(raw); err == nil {
				c.clientFails = 0
				return gpu, c.gpuApps(raw, c.clock()), nil
			}
		}
		c.gpuPrev, c.gpuPrevAt = nil, time.Time{}
		if c.clientFails++; c.clientFails >= clientAttempts {
			c.clientsOff = true
		}
		if err := ctx.Err(); err != nil {
			return GPUMetrics{}, nil, err
		}
	}
	raw, err := c.fetch(ctx, "IOAccelerator")
	if err != nil {
		return GPUMetrics{}, nil, fmt.Errorf("detector: gpu: %w", err)
	}
	gpu, err := ParseIOAcceleratorPlist(raw)
	if err != nil {
		return GPUMetrics{}, nil, fmt.Errorf("detector: gpu: %w", err)
	}
	return gpu, nil, nil
}

func (c *DarwinCollector) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// readHardware returns the hardware model, reading it with sysctl until it
// succeeds or hardwareAttempts calls have failed. Until then, and for good
// after that, the model is unknown and the core count is runtime.NumCPU
// (physical cores on Apple Silicon): a laptop then classifies as fanless.
func (c *DarwinCollector) readHardware(ctx context.Context) HardwareInfo {
	if !c.hwKnown && c.hardware != nil && c.hwFails < hardwareAttempts {
		if hw, err := c.hardware(ctx); err == nil {
			c.hw, c.hwKnown = hw, true
		} else {
			c.hwFails++
		}
	}
	if c.hwKnown {
		return c.hw
	}
	return HardwareInfo{PhysicalCPUs: runtime.NumCPU()}
}

// readThermal returns the current thermal pressure level, degrading as
// Collect documents.
func (c *DarwinCollector) readThermal(ctx context.Context) int {
	if c.thermal == nil {
		return 0
	}
	level, err := c.thermal(ctx)
	if err == nil {
		c.thermalLevel, c.thermalFails = level, 0
		return level
	}
	if c.thermalFails < thermalStaleTicks {
		c.thermalFails++
	}
	if c.thermalFails >= thermalStaleTicks {
		c.thermalLevel = 0
	}
	return c.thermalLevel
}
