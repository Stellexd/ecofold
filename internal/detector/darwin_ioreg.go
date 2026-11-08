//go:build darwin

package detector

import (
	"bytes"
	"fmt"
	"os/exec"

	"howett.net/plist"
)

// BatteryMetrics groups raw values extracted from the IOKit AppleSmartBattery
// node.
type BatteryMetrics struct {
	HasBattery       bool
	OnAC             bool
	IsCharging       bool
	BatteryPct       int
	VoltageMV        int
	AmperageMA       int64
	SystemLoadMW     int64
	BatteryPowerMW   int64
	WallInputMW      int64
	TimeRemainingMin *int
}

// GPUMetrics groups raw values extracted from the IOKit IOAccelerator node.
type GPUMetrics struct {
	GPUClass           string
	GPUDeviceUtilPct   int
	GPURendererUtilPct int
	GPUTilerUtilPct    int
	GPUInUseMemMiB     int
}

// region plist decoding structs

// ioregBatteryArray is the root returned by ioreg -a -r -d 1 -c AppleSmartBattery.
type ioregBatteryArray []ioregBatteryNode

type ioregBatteryNode struct {
	ExternalConnected bool   `plist:"ExternalConnected"`
	IsCharging        bool   `plist:"IsCharging"`
	CurrentCapacity   int    `plist:"CurrentCapacity"`
	Voltage           int    `plist:"Voltage"`
	InstantAmperage   uint64 `plist:"InstantAmperage"`
	Amperage          uint64 `plist:"Amperage"`
	TimeRemaining     int    `plist:"TimeRemaining"`

	BatteryData        batteryDataPlist        `plist:"BatteryData"`
	PowerTelemetryData powerTelemetryDataPlist `plist:"PowerTelemetryData"`
	PowerDistribution  powerDistributionPlist  `plist:"PowerDistribution"`
}

type batteryDataPlist struct {
	BatteryPower *uint64 `plist:"BatteryPower"`
}

type powerTelemetryDataPlist struct {
	SystemLoad *uint64 `plist:"SystemLoad"`
}

type powerDistributionPlist struct {
	IPDInputPower *uint64 `plist:"IPDInputPower"`
}

// ioregGPUArray is the root returned by ioreg -a -r -d 1 -c IOAccelerator.
type ioregGPUArray []ioregGPUNode

type ioregGPUNode struct {
	IOClass               string            `plist:"IOClass"`
	Model                 string            `plist:"model"`
	PerformanceStatistics map[string]uint64 `plist:"PerformanceStatistics"`
}

// endregion

// ParseSmartBatteryPlist parses the AppleSmartBattery XML output. If the plist
// contains an empty array (a machine without a battery), it returns
// HasBattery=false without an error.
func ParseSmartBatteryPlist(raw []byte) (BatteryMetrics, error) {
	var nodes ioregBatteryArray
	dec := plist.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&nodes); err != nil {
		return BatteryMetrics{}, fmt.Errorf("decode AppleSmartBattery plist: %w", err)
	}

	if len(nodes) == 0 {
		return BatteryMetrics{
			HasBattery: false,
			OnAC:       true,
			BatteryPct: 100,
		}, nil
	}

	n := nodes[0]

	// IOKit stores some signed values as uint64 in two's-complement form.
	instantAmperage := int64(n.InstantAmperage)
	if instantAmperage == 0 {
		instantAmperage = int64(n.Amperage)
	}

	var batteryPower int64
	if n.BatteryData.BatteryPower != nil {
		batteryPower = int64(*n.BatteryData.BatteryPower)
	} else {
		batteryPower = roundedMW(n.Voltage, instantAmperage)
	}

	var systemLoad int64
	if n.PowerTelemetryData.SystemLoad != nil {
		systemLoad = int64(*n.PowerTelemetryData.SystemLoad)
	}

	var wallInput int64
	if n.PowerDistribution.IPDInputPower != nil {
		wallInput = int64(*n.PowerDistribution.IPDInputPower)
	}

	onAC := n.ExternalConnected || wallInput > 0
	isCharging := n.IsCharging || instantAmperage > 0

	var timeRemaining *int
	if n.TimeRemaining != 0 && n.TimeRemaining != 65535 {
		tr := n.TimeRemaining
		timeRemaining = &tr
	}

	return BatteryMetrics{
		HasBattery:       true,
		OnAC:             onAC,
		IsCharging:       isCharging,
		BatteryPct:       n.CurrentCapacity,
		VoltageMV:        n.Voltage,
		AmperageMA:       instantAmperage,
		SystemLoadMW:     systemLoad,
		BatteryPowerMW:   batteryPower,
		WallInputMW:      wallInput,
		TimeRemainingMin: timeRemaining,
	}, nil
}

// ParseIOAcceleratorPlist parses the IOAccelerator XML output and searches for
// the first node containing PerformanceStatistics, without depending on a
// specific GPU class.
func ParseIOAcceleratorPlist(raw []byte) (GPUMetrics, error) {
	var nodes ioregGPUArray
	dec := plist.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&nodes); err != nil {
		return GPUMetrics{}, fmt.Errorf("decode IOAccelerator plist: %w", err)
	}

	for _, n := range nodes {
		if len(n.PerformanceStatistics) == 0 {
			continue
		}

		gpuClass := n.IOClass
		if gpuClass == "" {
			gpuClass = n.Model
		}

		return GPUMetrics{
			GPUClass:           gpuClass,
			GPUDeviceUtilPct:   int(n.PerformanceStatistics["Device Utilization %"]),
			GPURendererUtilPct: int(n.PerformanceStatistics["Renderer Utilization %"]),
			GPUTilerUtilPct:    int(n.PerformanceStatistics["Tiler Utilization %"]),
			GPUInUseMemMiB:     int(n.PerformanceStatistics["In use system memory"] / 1024 / 1024),
		}, nil
	}

	return GPUMetrics{}, nil
}

// BuildSnapshotFromPlists combines the battery and GPU plists into a
// SystemSnapshot ready for the evaluator.
func BuildSnapshotFromPlists(battRaw, gpuRaw []byte) (SystemSnapshot, error) {
	batt, err := ParseSmartBatteryPlist(battRaw)
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("batería: %w", err)
	}

	gpu, err := ParseIOAcceleratorPlist(gpuRaw)
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("gpu: %w", err)
	}

	snap := SystemSnapshot{
		HasBattery:         batt.HasBattery,
		OnAC:               batt.OnAC,
		IsCharging:         batt.IsCharging,
		BatteryPct:         batt.BatteryPct,
		VoltageMV:          batt.VoltageMV,
		AmperageMA:         batt.AmperageMA,
		SystemLoadMW:       batt.SystemLoadMW,
		BatteryPowerMW:     batt.BatteryPowerMW,
		WallInputMW:        batt.WallInputMW,
		TimeRemainingMin:   batt.TimeRemainingMin,
		GPUClass:           gpu.GPUClass,
		GPUDeviceUtilPct:   gpu.GPUDeviceUtilPct,
		GPURendererUtilPct: gpu.GPURendererUtilPct,
		GPUTilerUtilPct:    gpu.GPUTilerUtilPct,
		GPUInUseMemMiB:     gpu.GPUInUseMemMiB,
	}

	return snap, nil
}

// FetchIOReg runs `ioreg -a -r -d 1 -c <class>` and returns the XML output.
// It is only available on darwin.
func FetchIOReg(class string) ([]byte, error) {
	out, err := exec.Command("ioreg", "-a", "-r", "-d", "1", "-c", class).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("ioreg %s: %w (stderr: %s)", class, err, string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("ioreg %s: %w", class, err)
	}
	return out, nil
}

// FetchSmartBattery runs ioreg for AppleSmartBattery and parses the result.
func FetchSmartBattery() (BatteryMetrics, error) {
	raw, err := FetchIOReg("AppleSmartBattery")
	if err != nil {
		return BatteryMetrics{}, err
	}
	return ParseSmartBatteryPlist(raw)
}

// FetchIOAccelerator runs ioreg for IOAccelerator and parses the result.
func FetchIOAccelerator() (GPUMetrics, error) {
	raw, err := FetchIOReg("IOAccelerator")
	if err != nil {
		return GPUMetrics{}, err
	}
	return ParseIOAcceleratorPlist(raw)
}

// roundedMW calculates round((voltage_mV * amperage_mA) / 1000.0), rounding to
// the nearest integer while preserving the sign.
func roundedMW(voltageMV int, amperageMA int64) int64 {
	product := int64(voltageMV) * amperageMA
	if product >= 0 {
		return (product + 500) / 1000
	}
	return (product - 500) / 1000
}
