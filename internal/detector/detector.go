// Package detector determines EcoFold's operating state from system telemetry:
// battery, wall power, and GPU utilization/memory.
//
// The logic is platform-agnostic: data structures and the evaluator share code
// between Linux and macOS, while telemetry collection is separated by build tags.
package detector

import (
	"errors"
	"sync"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// OperatingState represents the control decision EcoFold should apply.
type OperatingState string

const (
	// StateFolding indicates that folding can run on wall power. On Linux the
	// daemon applies the configured eco watt limit while folding.
	StateFolding OperatingState = "folding"

	// StateFoldingEco indicates that folding can run on battery with sufficient
	// charge (macOS). The Linux collector reports no battery, so it never
	// produces this state; the daemon treats it like StateFolding.
	StateFoldingEco OperatingState = "folding_eco"

	// StatePausedBattery indicates that folding must be paused because the battery
	// is low or folding on battery is not allowed.
	StatePausedBattery OperatingState = "paused_battery"

	// StatePausedLoad indicates that folding must be paused because sustained GPU
	// load was detected (for example, an active game).
	StatePausedLoad OperatingState = "paused_load"
)

// ErrPerProcessUtilUnsupported is returned by a collector when the GPU driver
// lists graphics processes but cannot attribute utilization to any of them.
// Without per-process samples a game cannot be told apart from Folding@home,
// so callers must treat game detection as unavailable rather than as "no
// load".
var ErrPerProcessUtilUnsupported = errors.New("detector: per-process GPU utilization not supported")

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
	// as load if GPUMemRequiresUtilPct is also met. Default: 1500. The evaluator
	// rejects values less than or equal to 0 or greater than 65536 (64 GiB).
	GPUMemThresholdMiB int

	// GPUMemRequiresUtilPct is the minimum GPU utilization (%) that must accompany
	// high memory use for it to count as load. It avoids false positives from
	// occupied unified memory while the GPU is idle. Default: 25.
	GPUMemRequiresUtilPct int

	// SustainedTicksRequired is the number of consecutive high-load measurements
	// required to declare StatePausedLoad. Default: 3. The evaluator rejects
	// values less than or equal to 0 or greater than 60.
	SustainedTicksRequired int

	// ComputeMemThresholdMiB is the GPU memory (MiB) a non-ignored compute
	// process must hold to count as contending with Folding@home. Contending
	// processes count as load without meeting GPUUtilThresholdPct, because the
	// FAH core takes most of the SM time from any other compute work (Blender
	// Cycles measured 0-20 % SM next to FahCore at 61-98 %). A process also
	// needs an SM sample above 0 within the last computeActivityTicks ticks,
	// so an idle app that keeps its CUDA context (Blender at rest: 437 MiB,
	// 0 %) does not pause folding. Linux only. Default: 256. Values <= 0 or
	// > 65536 are reset to the default.
	ComputeMemThresholdMiB int

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

// computeActivityTicks is how many ticks an SM sample above 0 keeps a compute
// process active. NVML attributes few samples to a process that competes with
// the FAH core (Blender rendering: 10 %, 0, 0, 20 %, 0, 0, 0, 7 %, 10 %), so a
// single tick would flicker.
const computeActivityTicks = 5

// maxGPUMemThresholdMiB bounds GPUMemThresholdMiB at 64 GiB. A larger value
// makes the memory rule unreachable on any consumer GPU and, combined with
// GPUUtilThresholdPct = 100, disables load pausing entirely (R3).
const maxGPUMemThresholdMiB = 65536

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
		ComputeMemThresholdMiB: 256,
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

	// GPUProcessSessions maps the PID of a GPUProcesses entry to the game
	// session it belongs to (detection layer 2, Linux only): a Steam launch or
	// a Wine/Proton .exe. Processes outside a game session have no entry. It
	// is nil when no GPU process belongs to a session and on macOS.
	GPUProcessSessions map[uint32]string

	// LiveGameSessions lists the game sessions seen by the collector that are
	// still running this tick. A load pause caused by a process of one of
	// these sessions is held until the session ends.
	LiveGameSessions []string

	// UserPauseMatches lists the running processes that match the user's
	// pause list (Linux only). While it is not empty, every tick counts as
	// load regardless of GPU use: the user asked for no folding while those
	// apps run.
	UserPauseMatches []string
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
	// heldSessions are the game sessions whose processes caused the current
	// load streak or pause. While one of them is live, the cooldown waits.
	heldSessions map[string]struct{}
	// tick counts Evaluate calls; computeSeen records, per PID, the last tick
	// with an SM sample above 0 from a compute process.
	tick        uint64
	computeSeen map[uint32]uint64
	cfg         Config
}

// maxHeldSessions bounds heldSessions. Only sessions with a process above the
// thresholds are added, so a handful is the realistic maximum.
const maxHeldSessions = 32

// NewEvaluator creates an evaluator with the given configuration. Invalid or
// overly conservative values are clamped to defaults: GPUUtilThresholdPct <= 25
// or > 100 is forced to 45 (a threshold above 100% could never be reached and
// would disable load pausing); GPUMemThresholdMiB <= 0 or > 65536 is forced to
// 1500;
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
	if cfg.GPUMemThresholdMiB <= 0 || cfg.GPUMemThresholdMiB > maxGPUMemThresholdMiB {
		cfg.GPUMemThresholdMiB = DefaultConfig().GPUMemThresholdMiB
	}
	if cfg.ComputeMemThresholdMiB <= 0 || cfg.ComputeMemThresholdMiB > maxGPUMemThresholdMiB {
		cfg.ComputeMemThresholdMiB = DefaultConfig().ComputeMemThresholdMiB
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
// elapsed. Any load tick during the cooldown restarts it. If a process that
// counted as load belongs to a game session (GPUProcessSessions), the cooldown
// does not start while that session is in LiveGameSessions: a game in a menu
// or loading screen keeps folding paused, and the cooldown starts when the
// game exits. Sessions never start a pause on their own. The cooldown does not
// delay StatePausedBattery or its recovery. If the battery itself demands a
// pause while the load latch is active, StatePausedBattery is returned instead
// (it has no cooldown and reflects the independent, higher-priority reason).
//
// Evaluate is safe for concurrent use by multiple goroutines.
func (e *Evaluator) Evaluate(s SystemSnapshot) OperatingState {
	e.mu.Lock()
	defer e.mu.Unlock()

	batteryPause := s.HasBattery && !s.OnAC && (!e.cfg.AllowBatteryFolding || s.BatteryPct < e.cfg.MinBatteryPct)
	isHighLoad, sessions := e.isHighLoad(s)

	switch {
	case isHighLoad:
		e.lowLoadStreak = 0
		if e.highLoadStreak < e.cfg.SustainedTicksRequired {
			e.highLoadStreak++
		}
		if e.highLoadStreak >= e.cfg.SustainedTicksRequired {
			e.loadPaused = true
		}
		e.holdSessions(sessions)
	case e.loadPaused:
		e.highLoadStreak = 0
		if e.sessionHeld(s.LiveGameSessions) {
			e.lowLoadStreak = 0
			break
		}
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
		e.heldSessions = nil
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

// isHighLoad reports whether the snapshot shows sustained-capable game load and
// returns the game sessions of the processes that counted as load.
// When GPUProcesses is non-nil the global device fields are ignored: the
// collector is responsible for per-process attribution and device-wide metrics
// are contaminated by Folding@home. When GPUProcesses is nil the macOS/global
// path is used. It must be called with e.mu held.
func (e *Evaluator) isHighLoad(s SystemSnapshot) (bool, []string) {
	cfg := e.cfg
	e.tick++
	high := len(s.UserPauseMatches) > 0
	if s.GPUProcesses != nil {
		var sessions []string
		seen := make(map[uint32]bool, len(s.GPUProcesses))
		for _, proc := range s.GPUProcesses {
			if !proc.SMUtilValid {
				continue
			}
			if proc.SMUtilPct >= cfg.GPUUtilThresholdPct ||
				(proc.UsedVRAMMiB >= uint64(cfg.GPUMemThresholdMiB) &&
					proc.SMUtilPct >= cfg.GPUMemRequiresUtilPct) ||
				e.computeContending(proc, seen) {
				high = true
				if key, ok := s.GPUProcessSessions[proc.PID]; ok {
					sessions = append(sessions, key)
				}
			}
		}
		for pid := range e.computeSeen {
			if !seen[pid] {
				delete(e.computeSeen, pid)
			}
		}
		return high, sessions
	}
	if high {
		return true, nil
	}
	return s.GPUDeviceUtilPct >= cfg.GPUUtilThresholdPct ||
		(s.GPUInUseMemMiB >= cfg.GPUMemThresholdMiB &&
			s.GPUDeviceUtilPct >= cfg.GPUMemRequiresUtilPct), nil
}

// computeContending reports whether proc is compute work held back by the FAH
// core: a compute process with at least ComputeMemThresholdMiB that had an SM
// sample above 0 within the last computeActivityTicks ticks. seen collects
// the compute PIDs of this tick so stale entries can be dropped. It must be
// called with e.mu held.
func (e *Evaluator) computeContending(proc gpu.ProcessInfo, seen map[uint32]bool) bool {
	if !proc.Compute {
		return false
	}
	seen[proc.PID] = true
	if proc.SMUtilPct > 0 {
		if e.computeSeen == nil {
			e.computeSeen = make(map[uint32]uint64)
		}
		e.computeSeen[proc.PID] = e.tick
	}
	last, ok := e.computeSeen[proc.PID]
	return ok && e.tick-last < computeActivityTicks &&
		proc.UsedVRAMMiB >= uint64(e.cfg.ComputeMemThresholdMiB)
}

// holdSessions records the sessions of the processes that counted as load. It
// must be called with e.mu held.
func (e *Evaluator) holdSessions(sessions []string) {
	for _, key := range sessions {
		if e.heldSessions == nil {
			e.heldSessions = make(map[string]struct{})
		}
		if len(e.heldSessions) >= maxHeldSessions {
			return
		}
		e.heldSessions[key] = struct{}{}
	}
}

// sessionHeld drops held sessions that are no longer live and reports whether
// any remains. It must be called with e.mu held.
func (e *Evaluator) sessionHeld(live []string) bool {
	if len(e.heldSessions) == 0 {
		return false
	}
	alive := make(map[string]struct{}, len(live))
	for _, key := range live {
		alive[key] = struct{}{}
	}
	for key := range e.heldSessions {
		if _, ok := alive[key]; !ok {
			delete(e.heldSessions, key)
		}
	}
	return len(e.heldSessions) > 0
}
