// Package detector determines EcoFold's operating state from system telemetry:
// battery, wall power, and GPU utilization/memory.
//
// The logic is platform-agnostic: data structures and the evaluator share code
// between Linux and macOS, while telemetry collection is separated by build tags.
package detector

import (
	"sync"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// OperatingState represents the control decision EcoFold should apply.
type OperatingState string

const (
	// StateFolding indicates that folding can run at the normal power limit
	// (100% of TDP by default).
	StateFolding OperatingState = "folding"

	// StateFoldingEco indicates that folding can run, but a low-power profile is
	// recommended. On macOS this is used when folding with sufficient battery;
	// on Linux, the daemon applies the configured watt limit.
	StateFoldingEco OperatingState = "folding_eco"

	// StatePausedBattery indicates that folding must be paused because the battery
	// is low or folding on battery is not allowed.
	StatePausedBattery OperatingState = "paused_battery"

	// StatePausedLoad indicates that folding must be paused because sustained GPU
	// load was detected (for example, an active game).
	StatePausedLoad OperatingState = "paused_load"
)

// Config groups the thresholds that govern state decisions. The default values
// are documented and can be obtained with DefaultConfig.
type Config struct {
	// MinBatteryPct is the minimum battery percentage required to continue folding
	// on battery. Default: 50. Warning: constructing Config{AllowBatteryFolding:
	// true} without starting from DefaultConfig leaves this field at 0%, disabling
	// the battery reserve.
	MinBatteryPct int

	// AllowBatteryFolding indicates whether folding on battery is allowed. Default:
	// true.
	AllowBatteryFolding bool

	// GPUUtilThresholdPct is the GPU utilization threshold (%). Meeting or
	// exceeding it for the configured number of ticks counts as game load.
	// Default: 45. The evaluator rejects values less than or equal to 25 or
	// greater than 100.
	GPUUtilThresholdPct int

	// GPUMemThresholdMiB is the in-use GPU memory threshold (MiB). It only counts
	// as load if GPUMemRequiresUtilPct is also met. Default: 1500.
	GPUMemThresholdMiB int

	// GPUMemRequiresUtilPct is the minimum GPU utilization (%) that must accompany
	// high memory use for it to count as load. It avoids false positives from
	// occupied unified memory while the GPU is idle. Default: 25.
	GPUMemRequiresUtilPct int

	// SustainedTicksRequired is the number of consecutive high-load measurements
	// required to declare StatePausedLoad. Default: 3. The evaluator rejects
	// values less than or equal to 0 or greater than 60.
	SustainedTicksRequired int

	// CooldownTicksRequired is the number of consecutive load-free measurements
	// required before folding resumes after StatePausedLoad. It is the hysteresis
	// that stops folding from resuming the second a game releases the GPU.
	// Default: 60 (two minutes at the documented 2 s tick). Values below 60 are
	// raised to 60 and values above 90 are clamped to 90, so the cooldown is
	// always between two and three minutes; it cannot be disabled through this
	// field.
	CooldownTicksRequired int
}

// maxSustainedTicksRequired bounds SustainedTicksRequired. At one evaluation
// per second it is one minute of sustained load; larger values would make the
// pause effectively unreachable.
const maxSustainedTicksRequired = 60

// Cooldown bounds. The daemon ticks every 2 s, so [60,90] ticks is the
// documented two-to-three-minute cooldown.
const (
	minCooldownTicksRequired = 60
	maxCooldownTicksRequired = 90
)

// DefaultConfig returns the configuration with the documented default values.
func DefaultConfig() Config {
	return Config{
		MinBatteryPct:          50,
		AllowBatteryFolding:    true,
		GPUUtilThresholdPct:    45,
		GPUMemThresholdMiB:     1500,
		GPUMemRequiresUtilPct:  25,
		SustainedTicksRequired: 3,
		CooldownTicksRequired:  60,
	}
}

// SystemSnapshot is a unified snapshot of system telemetry. It is built from
// platform-specific providers.
type SystemSnapshot struct {
	// HasBattery indicates whether the machine has a battery. This is expected to
	// be false on desktop machines without a battery.
	HasBattery bool

	// OnAC indicates whether the machine is connected to AC power.
	OnAC bool

	// IsCharging indicates whether the battery is actively charging. A plugged-in
	// machine with a full battery may have OnAC=true and IsCharging=false.
	IsCharging bool

	// BatteryPct is the current battery percentage (0-100).
	BatteryPct int

	// VoltageMV is the battery voltage in millivolts. It must be 0 on machines
	// without a battery.
	VoltageMV int

	// AmperageMA is the battery current in milliamps. It may be negative while
	// discharging.
	AmperageMA int64

	// SystemLoadMW is the estimated system consumption in milliwatts.
	SystemLoadMW int64

	// BatteryPowerMW is the net battery power in milliwatts. Positive values
	// indicate charging; negative values indicate discharging.
	BatteryPowerMW int64

	// WallInputMW is the input power from the wall adapter in milliwatts.
	WallInputMW int64

	// TimeRemainingMin points to the estimated remaining minutes when known; nil
	// otherwise.
	TimeRemainingMin *int

	// GPUClass describes the class/name of the detected GPU node (for example,
	// "AGXAcceleratorG16G" on Apple Silicon).
	GPUClass string

	// GPUDeviceUtilPct is the overall utilization of the GPU device (%).
	GPUDeviceUtilPct int

	// GPURendererUtilPct is the utilization of the GPU renderer (%).
	GPURendererUtilPct int

	// GPUTilerUtilPct is the utilization of the GPU tiler (%).
	GPUTilerUtilPct int

	// GPUInUseMemMiB is the system memory in use by the GPU, in MiB.
	GPUInUseMemMiB int

	// GPUProcesses lists per-process GPU activity when the platform can
	// attribute it (Linux/NVML). It is nil on platforms that only report
	// device-wide metrics, and the evaluator then falls back to the global
	// fields above.
	//
	// On Linux the collector must leave GPUDeviceUtilPct and GPUInUseMemMiB at
	// zero: the Folding@home core keeps the device busy, so those aggregates are
	// contaminated and would pause folding permanently.
	GPUProcesses []gpu.ProcessInfo
}

// Evaluator decides the operating state based on consecutive snapshots. It does
// not make system calls; it only applies business logic.
//
// An Evaluator is safe for concurrent use by multiple goroutines.
type Evaluator struct {
	mu             sync.Mutex
	highLoadStreak int
	lowLoadStreak  int
	loadPaused     bool
	cfg            Config
}

// NewEvaluator creates an evaluator with the given configuration. Invalid or
// overly conservative values are clamped to defaults: GPUUtilThresholdPct <= 25
// or > 100 is forced to 45 (a threshold above 100% could never be reached and
// would disable load pausing); GPUMemThresholdMiB <= 0 is forced to 1500;
// SustainedTicksRequired <= 0 or > 60 is forced to 3; GPUMemRequiresUtilPct below 25 or
// greater than GPUUtilThresholdPct is forced to 25 (never below 25);
// MinBatteryPct < 0 or > 100 is forced to 50; CooldownTicksRequired below 60 is
// raised to 60 and above 90 is clamped to 90.
//
// AllowBatteryFolding's zero value (false) is intentional and conservative: a
// Config{} never folds on battery; callers that want true must start from
// DefaultConfig().
func NewEvaluator(cfg Config) *Evaluator {
	if cfg.GPUUtilThresholdPct <= 25 || cfg.GPUUtilThresholdPct > 100 {
		cfg.GPUUtilThresholdPct = DefaultConfig().GPUUtilThresholdPct
	}
	if cfg.GPUMemThresholdMiB <= 0 {
		cfg.GPUMemThresholdMiB = DefaultConfig().GPUMemThresholdMiB
	}
	if cfg.SustainedTicksRequired <= 0 || cfg.SustainedTicksRequired > maxSustainedTicksRequired {
		cfg.SustainedTicksRequired = DefaultConfig().SustainedTicksRequired
	}
	if cfg.GPUMemRequiresUtilPct < 25 || cfg.GPUMemRequiresUtilPct > cfg.GPUUtilThresholdPct {
		cfg.GPUMemRequiresUtilPct = DefaultConfig().GPUMemRequiresUtilPct
	}
	if cfg.MinBatteryPct < 0 || cfg.MinBatteryPct > 100 {
		cfg.MinBatteryPct = DefaultConfig().MinBatteryPct
	}
	if cfg.CooldownTicksRequired < minCooldownTicksRequired {
		cfg.CooldownTicksRequired = DefaultConfig().CooldownTicksRequired
	}
	if cfg.CooldownTicksRequired > maxCooldownTicksRequired {
		cfg.CooldownTicksRequired = maxCooldownTicksRequired
	}
	return &Evaluator{cfg: cfg}
}

// Evaluate classifies the snapshot into one of the operating states. The
// priority logic is:
//
//  1. Sustained GPU game load → StatePausedLoad.
//     Load is detected per process when GPUProcesses is populated (Linux):
//     a process counts if its own SM utilization meets or exceeds the
//     threshold, or if its own in-use memory meets the memory threshold AND
//     its own utilization meets GPUMemRequiresUtilPct. Memory from one process
//     is never combined with another process's utilization. When GPUProcesses
//     is nil, the device-wide fields are used with the same rule.
//  2. Low battery or battery folding disabled → StatePausedBattery.
//  3. On battery with sufficient charge and folding allowed → StateFoldingEco.
//  4. Any other case → StateFolding.
//
// After a load pause, the evaluator latches the decision and keeps returning
// StatePausedLoad until CooldownTicksRequired consecutive load-free ticks have
// elapsed. Any load tick during the cooldown restarts it. The cooldown does not
// delay StatePausedBattery or its recovery. If the battery itself demands a
// pause while the load latch is active, StatePausedBattery is returned instead
// (it has no cooldown and reflects the independent, higher-priority reason).
//
// Evaluate is safe for concurrent use by multiple goroutines.
func (e *Evaluator) Evaluate(s SystemSnapshot) OperatingState {
	e.mu.Lock()
	defer e.mu.Unlock()

	batteryPause := s.HasBattery && !s.OnAC && (!e.cfg.AllowBatteryFolding || s.BatteryPct < e.cfg.MinBatteryPct)
	isHighLoad := e.isHighLoad(s)

	switch {
	case isHighLoad:
		e.lowLoadStreak = 0
		if e.highLoadStreak < e.cfg.SustainedTicksRequired {
			e.highLoadStreak++
		}
		if e.highLoadStreak >= e.cfg.SustainedTicksRequired {
			e.loadPaused = true
		}
	case e.loadPaused:
		e.highLoadStreak = 0
		if e.lowLoadStreak < e.cfg.CooldownTicksRequired {
			e.lowLoadStreak++
		}
		if e.lowLoadStreak >= e.cfg.CooldownTicksRequired {
			e.loadPaused = false
			e.lowLoadStreak = 0
		}
	default:
		e.highLoadStreak = 0
		e.lowLoadStreak = 0
	}

	if e.loadPaused {
		// A current high-load tick keeps reporting the load reason even if the
		// battery is also low. If the load is gone but the battery still demands
		// a pause, report that independent reason instead.
		if !isHighLoad && batteryPause {
			return StatePausedBattery
		}
		return StatePausedLoad
	}

	if batteryPause {
		return StatePausedBattery
	}
	if s.HasBattery && !s.OnAC {
		return StateFoldingEco
	}

	return StateFolding
}

// isHighLoad reports whether the snapshot shows sustained-capable game load.
// When GPUProcesses is non-nil the global device fields are ignored: the
// collector is responsible for per-process attribution and device-wide metrics
// are contaminated by Folding@home. When GPUProcesses is nil the macOS/global
// path is used. It must be called with e.mu held.
func (e *Evaluator) isHighLoad(s SystemSnapshot) bool {
	cfg := e.cfg
	if s.GPUProcesses != nil {
		for _, proc := range s.GPUProcesses {
			if !proc.SMUtilValid {
				continue
			}
			if proc.SMUtilPct >= cfg.GPUUtilThresholdPct {
				return true
			}
			if proc.UsedVRAMMiB >= uint64(cfg.GPUMemThresholdMiB) &&
				proc.SMUtilPct >= cfg.GPUMemRequiresUtilPct {
				return true
			}
		}
		return false
	}
	return s.GPUDeviceUtilPct >= cfg.GPUUtilThresholdPct ||
		(s.GPUInUseMemMiB >= cfg.GPUMemThresholdMiB &&
			s.GPUDeviceUtilPct >= cfg.GPUMemRequiresUtilPct)
}
