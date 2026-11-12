//go:build linux && cgo

package gpu

import (
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// NvidiaProvider uses the NVIDIA NVML library to manage a single GPU (index 0).
//
// NVML is process-global: nvml.Init and nvml.Shutdown affect the whole
// process, not one handle. The package therefore keeps a reference count so
// several providers can coexist, the last Shutdown performs the real
// nvml.Shutdown, and a stray Shutdown never tears NVML down under another
// provider.
type NvidiaProvider struct {
	mu          sync.Mutex
	device      nvml.Device
	initialized bool
	// lastSeenUtil is the high-water timestamp passed to
	// GetProcessUtilization so each call returns only fresh samples.
	lastSeenUtil uint64
}

var (
	nvmlMu   sync.Mutex
	nvmlRefs int
)

// acquireNVML increments the process-wide NVML reference count, initializing
// the library on the first reference. The caller must pair it with
// releaseNVML.
func acquireNVML() error {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	if nvmlRefs > 0 {
		nvmlRefs++
		return nil
	}
	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return fmtError("nvml.Init", ret)
	}
	nvmlRefs = 1
	return nil
}

// releaseNVML drops one reference and shuts NVML down when the last one goes
// away. The count only reaches zero after a successful shutdown, so a failed
// nvml.Shutdown can be retried by calling releaseNVML again.
func releaseNVML() error {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	switch {
	case nvmlRefs == 0:
		return nil
	case nvmlRefs > 1:
		nvmlRefs--
		return nil
	}
	if ret := nvml.Shutdown(); ret != nvml.SUCCESS {
		return fmtError("nvml.Shutdown", ret)
	}
	nvmlRefs = 0
	return nil
}

// Init initializes NVML and opens GPU handle 0. It is idempotent: calling it
// on an already initialized provider is a no-op, so it never leaks an NVML
// reference.
func (p *NvidiaProvider) Init() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.initialized {
		return nil
	}
	if err := acquireNVML(); err != nil {
		return err
	}

	dev, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		_ = releaseNVML()
		return fmtError("nvml.DeviceGetHandleByIndex(0)", ret)
	}
	p.device = dev
	p.initialized = true
	p.lastSeenUtil = 0
	return nil
}

// Shutdown releases the provider's NVML resources. It is idempotent and a
// no-op before Init, so it never shuts NVML down on behalf of another
// provider.
func (p *NvidiaProvider) Shutdown() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.initialized {
		return nil
	}
	p.device = nil
	p.initialized = false
	p.lastSeenUtil = 0
	return releaseNVML()
}

// GetTelemetry reads the current GPU telemetry snapshot.
func (p *NvidiaProvider) GetTelemetry() (Telemetry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var t Telemetry
	if p.device == nil {
		return t, fmt.Errorf("gpu: provider not initialized")
	}

	name, ret := p.device.GetName()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetName", ret)
	}
	uuid, ret := p.device.GetUUID()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetUUID", ret)
	}
	temp, ret := p.device.GetTemperature(nvml.TEMPERATURE_GPU)
	if ret != nvml.SUCCESS {
		return t, fmtError("GetTemperature", ret)
	}
	powerUsage, ret := p.device.GetPowerUsage()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetPowerUsage", ret)
	}
	powerLimit, ret := p.device.GetPowerManagementLimit()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetPowerManagementLimit", ret)
	}
	minLimit, maxLimit, ret := p.device.GetPowerManagementLimitConstraints()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetPowerManagementLimitConstraints", ret)
	}
	defaultLimit, ret := p.device.GetPowerManagementDefaultLimit()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetPowerManagementDefaultLimit", ret)
	}
	mem, ret := p.device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		return t, fmtError("GetMemoryInfo", ret)
	}

	minW, maxW, settable := powerRangeW(minLimit, maxLimit)
	t = Telemetry{
		Name:               name,
		UUID:               uuid,
		TempC:              int(temp),
		PowerUsageW:        mwToW(powerUsage),
		PowerLimitW:        mwToW(powerLimit),
		MinPowerLimitW:     minW,
		MaxPowerLimitW:     maxW,
		DefaultPowerLimitW: mwToW(defaultLimit),
		UsedVRAMMiB:        int(bytesToMiB(mem.Used)),
		TotalVRAMMiB:       int(bytesToMiB(mem.Total)),
		SupportsPowerLimit: settable,
	}
	return t, nil
}

// SetPowerLimit validates the requested wattage and applies it via NVML (mW).
func (p *NvidiaProvider) SetPowerLimit(watts int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.device == nil {
		return fmt.Errorf("gpu: provider not initialized")
	}
	minLimit, maxLimit, ret := p.device.GetPowerManagementLimitConstraints()
	if ret != nvml.SUCCESS {
		return fmtError("GetPowerManagementLimitConstraints", ret)
	}
	mw, err := wToMW(watts)
	if err != nil {
		return err
	}
	if mw < minLimit || mw > maxLimit {
		return fmt.Errorf("power limit %dW (%dmW) outside allowed range [%dmW, %dmW]", watts, mw, minLimit, maxLimit)
	}
	ret = p.device.SetPowerManagementLimit(mw)
	if ret != nvml.SUCCESS {
		return fmtError("SetPowerManagementLimit", ret)
	}
	return nil
}

// RestoreDefaultPowerLimit resets the power limit to the device's default,
// clamped to the current constraints so the restore never fails on a value
// NVML would reject.
func (p *NvidiaProvider) RestoreDefaultPowerLimit() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.device == nil {
		return fmt.Errorf("gpu: provider not initialized")
	}
	defaultLimit, ret := p.device.GetPowerManagementDefaultLimit()
	if ret != nvml.SUCCESS {
		return fmtError("GetPowerManagementDefaultLimit", ret)
	}
	minLimit, maxLimit, ret := p.device.GetPowerManagementLimitConstraints()
	if ret != nvml.SUCCESS {
		return fmtError("GetPowerManagementLimitConstraints", ret)
	}
	if minLimit > maxLimit || maxLimit == 0 {
		return fmt.Errorf("invalid power limit constraints [%dmW, %dmW]", minLimit, maxLimit)
	}
	ret = p.device.SetPowerManagementLimit(clampMW(defaultLimit, minLimit, maxLimit))
	if ret != nvml.SUCCESS {
		return fmtError("SetPowerManagementLimit", ret)
	}
	return nil
}

// GetGraphicsProcesses returns the list of graphics processes running on the
// GPU, enriched with per-process SM utilization when NVML can attribute it.
// The device-wide utilization is deliberately not used: the Folding@home core
// keeps the GPU busy, so only per-process samples can tell a game apart.
func (p *NvidiaProvider) GetGraphicsProcesses() ([]ProcessInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.device == nil {
		return nil, fmt.Errorf("gpu: provider not initialized")
	}
	procs, ret := p.device.GetGraphicsRunningProcesses()
	if ret != nvml.SUCCESS {
		return nil, fmtError("GetGraphicsRunningProcesses", ret)
	}

	utilByPID, utilValid, err := p.processUtilization()
	if err != nil {
		return nil, err
	}

	result := make([]ProcessInfo, 0, len(procs))
	for _, proc := range procs {
		info := ProcessInfo{
			PID:         proc.Pid,
			UsedVRAMMiB: bytesToMiB(proc.UsedGpuMemory),
			SMUtilValid: utilValid,
		}
		if utilValid {
			// A graphics process without a fresh sample was idle during the last
			// sampling window, so 0 is a valid measurement.
			info.SMUtilPct = utilByPID[proc.Pid]
		}
		result = append(result, info)
	}
	return result, nil
}

// processUtilization fetches per-process SM utilization since the last call.
// It reports utilValid=false when NVML does not support the query, so callers
// never confuse "unsupported" with "idle".
func (p *NvidiaProvider) processUtilization() (map[uint32]int, bool, error) {
	samples, ret := p.device.GetProcessUtilization(p.lastSeenUtil)
	switch ret {
	case nvml.SUCCESS:
		byPID, high := mergeUtilizationSamples(p.lastSeenUtil, samples)
		if high > p.lastSeenUtil {
			p.lastSeenUtil = high
		}
		return byPID, true, nil
	case nvml.ERROR_NOT_FOUND, nvml.ERROR_INSUFFICIENT_SIZE:
		// No fresh samples: every process was idle in the last window.
		return nil, true, nil
	case nvml.ERROR_NOT_SUPPORTED:
		return nil, false, nil
	default:
		return nil, false, fmtError("GetProcessUtilization", ret)
	}
}

// mergeUtilizationSamples keeps the most recent sample per PID and returns the
// new high-water timestamp to pass to the next NVML call.
func mergeUtilizationSamples(lastSeen uint64, samples []nvml.ProcessUtilizationSample) (map[uint32]int, uint64) {
	type latest struct {
		ts   uint64
		util int
	}
	seen := make(map[uint32]latest, len(samples))
	high := lastSeen
	for _, s := range samples {
		if s.TimeStamp > high {
			high = s.TimeStamp
		}
		cur, ok := seen[s.Pid]
		if ok && s.TimeStamp < cur.ts {
			continue
		}
		seen[s.Pid] = latest{ts: s.TimeStamp, util: clampPercent(int(s.SmUtil))}
	}
	byPID := make(map[uint32]int, len(seen))
	for pid, l := range seen {
		byPID[pid] = l.util
	}
	return byPID, high
}

// clampPercent limits a percentage to [0,100].
func clampPercent(pct int) int {
	switch {
	case pct < 0:
		return 0
	case pct > 100:
		return 100
	}
	return pct
}

// NewProvider returns the Linux NVML-backed Provider.
func NewProvider() Provider {
	return &NvidiaProvider{}
}

func fmtError(op string, ret nvml.Return) error {
	return fmt.Errorf("%s failed: %s", op, nvml.ErrorString(ret))
}
