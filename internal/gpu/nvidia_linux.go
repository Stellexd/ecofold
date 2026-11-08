//go:build linux && cgo

package gpu

import (
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// NvidiaProvider uses the NVIDIA NVML library to manage a single GPU (index 0).
type NvidiaProvider struct {
	device nvml.Device
}

// Init initializes NVML and opens GPU handle 0.
func (p *NvidiaProvider) Init() error {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return fmtError("nvml.Init", ret)
	}

	dev, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		_ = nvml.Shutdown()
		return fmtError("nvml.DeviceGetHandleByIndex(0)", ret)
	}
	p.device = dev
	return nil
}

// Shutdown releases NVML resources.
func (p *NvidiaProvider) Shutdown() error {
	ret := nvml.Shutdown()
	if ret != nvml.SUCCESS {
		return fmtError("nvml.Shutdown", ret)
	}
	return nil
}

// GetTelemetry reads the current GPU telemetry snapshot.
func (p *NvidiaProvider) GetTelemetry() (Telemetry, error) {
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

	t = Telemetry{
		Name:               name,
		UUID:               uuid,
		TempC:              int(temp),
		PowerUsageW:        mwToW(powerUsage),
		PowerLimitW:        mwToW(powerLimit),
		MinPowerLimitW:     mwToW(minLimit),
		MaxPowerLimitW:     mwToW(maxLimit),
		DefaultPowerLimitW: mwToW(defaultLimit),
		UsedVRAMMiB:        int(bytesToMiB(mem.Used)),
		TotalVRAMMiB:       int(bytesToMiB(mem.Total)),
		SupportsPowerLimit: true,
	}
	return t, nil
}

// SetPowerLimit validates the requested wattage and applies it via NVML (mW).
func (p *NvidiaProvider) SetPowerLimit(watts int) error {
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

// RestoreDefaultPowerLimit resets the power limit to the device's default.
func (p *NvidiaProvider) RestoreDefaultPowerLimit() error {
	if p.device == nil {
		return fmt.Errorf("gpu: provider not initialized")
	}
	defaultLimit, ret := p.device.GetPowerManagementDefaultLimit()
	if ret != nvml.SUCCESS {
		return fmtError("GetPowerManagementDefaultLimit", ret)
	}
	ret = p.device.SetPowerManagementLimit(defaultLimit)
	if ret != nvml.SUCCESS {
		return fmtError("SetPowerManagementLimit", ret)
	}
	return nil
}

// GetGraphicsProcesses returns the list of graphics processes running on the GPU.
func (p *NvidiaProvider) GetGraphicsProcesses() ([]ProcessInfo, error) {
	if p.device == nil {
		return nil, fmt.Errorf("gpu: provider not initialized")
	}
	procs, ret := p.device.GetGraphicsRunningProcesses()
	if ret != nvml.SUCCESS {
		return nil, fmtError("GetGraphicsRunningProcesses", ret)
	}

	result := make([]ProcessInfo, 0, len(procs))
	for _, proc := range procs {
		result = append(result, ProcessInfo{
			PID:         proc.Pid,
			UsedVRAMMiB: bytesToMiB(proc.UsedGpuMemory),
		})
	}
	return result, nil
}

// NewProvider returns the Linux NVML-backed Provider.
func NewProvider() Provider {
	return &NvidiaProvider{}
}

func fmtError(op string, ret nvml.Return) error {
	return fmt.Errorf("%s failed: %s", op, nvml.ErrorString(ret))
}
