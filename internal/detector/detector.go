// Package detector determines EcoFold's operating state from system telemetry:
// battery, wall power, and GPU utilization/memory.
//
// The logic is platform-agnostic: data structures and the evaluator share code
// between Linux and macOS, while telemetry collection is separated by build tags.
package detector

import "sync"

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
}

// maxSustainedTicksRequired bounds SustainedTicksRequired. At one evaluation
// per second it is one minute of sustained load; larger values would make the
// pause effectively unreachable.
const maxSustainedTicksRequired = 60

// DefaultConfig returns the configuration with the documented default values.
func DefaultConfig() Config {
	return Config{
		MinBatteryPct:          50,
		AllowBatteryFolding:    true,
		GPUUtilThresholdPct:    45,
		GPUMemThresholdMiB:     1500,
		GPUMemRequiresUtilPct:  25,
		SustainedTicksRequired: 3,
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
}

// Evaluator decides the operating state based on consecutive snapshots. It does
// not make system calls; it only applies business logic.
//
// An Evaluator is safe for concurrent use by multiple goroutines.
type Evaluator struct {
	mu             sync.Mutex
	highLoadStreak int
	cfg            Config
}

// NewEvaluator creates an evaluator with the given configuration. Invalid or
// overly conservative values are clamped to defaults: GPUUtilThresholdPct <= 25
// or > 100 is forced to 45 (a threshold above 100% could never be reached and
// would disable load pausing); GPUMemThresholdMiB <= 0 is forced to 1500;
// SustainedTicksRequired <= 0 or > 60 is forced to 3; GPUMemRequiresUtilPct below 25 or
// greater than GPUUtilThresholdPct is forced to 25 (never below 25);
// MinBatteryPct < 0 or > 100 is forced to 50.
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
	return &Evaluator{cfg: cfg}
}

// Evaluate classifies the snapshot into one of the operating states. The
// priority logic is:
//
//  1. Sustained GPU game load → StatePausedLoad.
//     Load is detected if overall utilization meets or exceeds the
//     threshold, or if in-use memory meets or exceeds its threshold AND
//     overall utilization meets or exceeds GPUMemRequiresUtilPct. This
//     avoids false positives from occupied unified memory while the GPU is
//     idle.
//  2. Low battery or battery folding disabled → StatePausedBattery.
//  3. On battery with sufficient charge and folding allowed → StateFoldingEco.
//  4. Any other case → StateFolding.
//
// Evaluate is safe for concurrent use by multiple goroutines.
func (e *Evaluator) Evaluate(s SystemSnapshot) OperatingState {
	e.mu.Lock()
	defer e.mu.Unlock()

	isHighLoad := s.GPUDeviceUtilPct >= e.cfg.GPUUtilThresholdPct ||
		(s.GPUInUseMemMiB >= e.cfg.GPUMemThresholdMiB &&
			s.GPUDeviceUtilPct >= e.cfg.GPUMemRequiresUtilPct)

	if isHighLoad {
		if e.highLoadStreak < e.cfg.SustainedTicksRequired {
			e.highLoadStreak++
		}
	} else {
		e.highLoadStreak = 0
	}

	if e.highLoadStreak >= e.cfg.SustainedTicksRequired {
		return StatePausedLoad
	}

	if s.HasBattery && !s.OnAC {
		if !e.cfg.AllowBatteryFolding || s.BatteryPct < e.cfg.MinBatteryPct {
			return StatePausedBattery
		}
		return StateFoldingEco
	}

	return StateFolding
}
