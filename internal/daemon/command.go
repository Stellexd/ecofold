package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
)

// Commands reach the loop through a channel and run on the loop goroutine,
// between ticks: the daemon's state, the GPU, FAH and the collector are never
// touched from the caller's goroutine, so none of them needs a lock.

const (
	// cmdQueue is the command channel's buffer.
	cmdQueue = 8
	// cmdTimeout is how long a caller waits for the loop to take a command,
	// and again for the loop to finish it.
	cmdTimeout = 3 * time.Second
	// maxPausePatterns caps the patterns of one SetPauseProcesses call,
	// before deduplication.
	maxPausePatterns = 64
)

// Errors returned by the command methods. Use errors.Is: they are wrapped
// with details.
var (
	// ErrBusy means the loop did not take or finish the command in time. A
	// command the loop never took is canceled and has no effect.
	ErrBusy = errors.New("daemon: control loop busy")
	// ErrStopping means the daemon is shutting down or not running.
	ErrStopping = errors.New("daemon: stopping")
	// ErrUnsupported means this platform or device cannot do what was asked.
	ErrUnsupported = errors.New("daemon: not supported")
	// ErrOutOfRange means the watts are outside the device's constraints.
	ErrOutOfRange = errors.New("daemon: power limit out of range")
	// ErrCapFailed means the eco power limit could not be applied.
	ErrCapFailed = errors.New("daemon: could not apply the eco power limit")
	// ErrGameActive means folding was not resumed because the evaluator's
	// current decision is a load pause.
	ErrGameActive = errors.New("daemon: a game or GPU load is active")
	// ErrOnBattery means folding was not resumed because the evaluator's
	// current decision is a battery pause.
	ErrOnBattery = errors.New("daemon: on battery")
	// ErrThermal means folding was not resumed because the evaluator's
	// current decision is a thermal pause.
	ErrThermal = errors.New("daemon: thermal pressure")
	// ErrFAHUnavailable means Folding@home did not confirm the action. The
	// loop retries it every tick.
	ErrFAHUnavailable = errors.New("daemon: folding@home did not confirm the action")
	// ErrInvalidPatterns means the pause list was rejected.
	ErrInvalidPatterns = errors.New("daemon: invalid pause list")
)

// Override is a manual command that outranks the evaluator.
type Override string

const (
	// OverrideNone leaves the evaluator in charge.
	OverrideNone Override = "none"
	// OverridePause keeps folding paused and the GPU at its default limit
	// until ForceResume.
	OverridePause Override = "pause"
)

// PauseListSetter is implemented by collectors that can match a pause list
// against running processes (LinuxCollector). A collector that cannot must
// not implement it: SetPauseProcesses then fails with ErrUnsupported. The
// daemon calls it only from the loop goroutine, between Collect calls.
type PauseListSetter interface {
	SetPauseProcesses(patterns []string)
}

// Status is an immutable snapshot of the daemon, published at the end of
// every tick and every command. Telemetry, game sessions and the FAH state
// are the last known values, as of UpdatedAt.
type Status struct {
	Mode Mode `json:"mode"`
	// ManualOverride is "pause" between ForcePause and ForceResume.
	ManualOverride Override `json:"manual_override"`
	// PausedByUs reports that the current FAH pause is the daemon's, so the
	// daemon resumes it; false while folding and for a pause the user made.
	PausedByUs bool `json:"paused_by_us"`
	// StartupHold reports that PausedByUs is a pause inherited from a
	// previous run that the startup hold still keeps (StartupHoldTicks).
	StartupHold bool `json:"startup_hold"`
	Degraded    bool `json:"degraded"`

	// PowerCapable is false when the device has no power limit control;
	// PowerEnabled is false when the daemon is not applying it (not capable,
	// no permission or Degraded).
	PowerCapable bool `json:"power_capable"`
	PowerEnabled bool `json:"power_enabled"`
	// CapApplied reports that the eco limit is, or may be, in effect.
	CapApplied         bool `json:"cap_applied"`
	PowerLimitW        int  `json:"power_limit_w"`
	DefaultPowerLimitW int  `json:"default_power_limit_w"`
	MinPowerLimitW     int  `json:"min_power_limit_w"`
	MaxPowerLimitW     int  `json:"max_power_limit_w"`
	EcoWatts           int  `json:"eco_watts"`

	GPU     GPUStatus     `json:"gpu"`
	Battery BatteryStatus `json:"battery"`
	FAH     FAHStatus     `json:"fah"`

	// GameSessions are the live game sessions (Linux detection layer 2).
	GameSessions []string `json:"game_sessions"`
	// PauseListSupported is false when the collector cannot match processes
	// (macOS): SetPauseProcesses then fails with ErrUnsupported.
	PauseListSupported bool `json:"pause_list_supported"`
	// PauseProcesses is the pause list; PauseMatches are the running
	// processes that match it.
	PauseProcesses []string `json:"pause_processes"`
	PauseMatches   []string `json:"pause_matches"`

	// MacProfile is the Mac's cooling class ("fanless", "active_laptop",
	// "desktop"); empty on Linux. RecommendedCPUs is the CPU thread budget
	// for Folding@home from the last snapshot; 0 means the daemon leaves
	// FAH's cpus alone (Linux).
	MacProfile      string `json:"mac_profile"`
	RecommendedCPUs int    `json:"recommended_cpus"`

	// FoldingSeconds is the time Folding@home was seen running a work unit
	// since the daemon started, and ProjectsContributed the projects of
	// those work units, in the order they were first seen. Both are kept in
	// memory only and start again with the daemon.
	FoldingSeconds      int64 `json:"folding_seconds"`
	ProjectsContributed []int `json:"projects_contributed"`

	UpdatedAt time.Time `json:"updated_at"`
}

// GPUStatus is the last GPU telemetry read. With a simulated provider (macOS,
// the Linux stub) the watts, temperature and VRAM are zero and Name comes
// from the collector: a mock's numbers are never published.
type GPUStatus struct {
	Name         string `json:"name"`
	TempC        int    `json:"temp_c"`
	PowerUsageW  int    `json:"power_usage_w"`
	UsedVRAMMiB  int    `json:"used_vram_mib"`
	TotalVRAMMiB int    `json:"total_vram_mib"`
	// UtilPct and InUseMemMiB are the device-wide metrics of the last
	// snapshot (macOS ioreg). They stay zero on Linux, where the collector
	// reports per-process data instead.
	UtilPct     int `json:"util_pct"`
	InUseMemMiB int `json:"in_use_mem_mib"`
}

// BatteryStatus is the battery state of the last snapshot.
type BatteryStatus struct {
	Present  bool `json:"present"`
	OnAC     bool `json:"on_ac"`
	Charging bool `json:"charging"`
	Pct      int  `json:"pct"`
}

// FAHStatus is what the daemon last learned about Folding@home. The loop
// reads the status again every fahRefreshTicks ticks, so a change made in FAH
// itself shows up here within that time.
type FAHStatus struct {
	Paused bool `json:"paused"`
	// State is one of the fah.State* values; "unknown" until FAH answers.
	State string `json:"state"`
	// ActiveProjectID is the project of the running work unit, or 0.
	ActiveProjectID int `json:"active_project_id"`
	// CPUs is FAH's "cpus" option (default group); 0 when not reported.
	CPUs int `json:"cpus"`
	// Reachable is false when the last call failed; the fields above then
	// hold the last known values.
	Reachable bool `json:"reachable"`
	// ObservedAt is the time of that call; zero means FAH was never asked.
	ObservedAt time.Time `json:"observed_at"`
}

func (s Status) clone() Status {
	s.GameSessions = append([]string{}, s.GameSessions...)
	s.PauseProcesses = append([]string{}, s.PauseProcesses...)
	s.PauseMatches = append([]string{}, s.PauseMatches...)
	s.ProjectsContributed = append([]int{}, s.ProjectsContributed...)
	return s
}

// Status returns the last published snapshot. It is safe for concurrent use
// and never waits for the loop.
func (d *Daemon) Status() Status {
	if st := d.status.Load(); st != nil {
		return st.clone()
	}
	return Status{Mode: ModeStarting, ManualOverride: OverrideNone, FAH: FAHStatus{State: fah.StateUnknown}}.clone()
}

// publish stores a snapshot of the current state. Loop goroutine only.
func (d *Daemon) publish() Status {
	if tel, err := d.gpu.GetTelemetry(); err == nil {
		d.powerLimitW = tel.PowerLimitW
		if tel.Simulated {
			// A mock's watts, temperature and VRAM are not this machine's.
			d.gpuStatus = GPUStatus{Name: d.gpuName}
		} else {
			d.gpuStatus = GPUStatus{
				Name:         tel.Name,
				TempC:        tel.TempC,
				PowerUsageW:  tel.PowerUsageW,
				UsedVRAMMiB:  tel.UsedVRAMMiB,
				TotalVRAMMiB: tel.TotalVRAMMiB,
			}
		}
	}
	d.gpuStatus.UtilPct, d.gpuStatus.InUseMemMiB = d.gpuUtilPct, d.gpuInUseMemMiB
	st := Status{
		Mode:                d.mode,
		ManualOverride:      d.manual,
		PausedByUs:          d.pausedByUs,
		StartupHold:         d.resumeHold > 0 && d.pausedByUs,
		Degraded:            d.degraded,
		PowerCapable:        d.powerCapable,
		PowerEnabled:        d.powerEnabled,
		CapApplied:          d.capApplied,
		PowerLimitW:         d.powerLimitW,
		DefaultPowerLimitW:  d.defaultW,
		MinPowerLimitW:      d.minW,
		MaxPowerLimitW:      d.maxW,
		EcoWatts:            d.ecoWatts,
		GPU:                 d.gpuStatus,
		Battery:             d.battery,
		FAH:                 d.fahStatus,
		GameSessions:        d.sessions,
		PauseListSupported:  d.pauseListSupported(),
		PauseProcesses:      d.pauseProcesses,
		PauseMatches:        d.pauseMatches,
		MacProfile:          string(d.macProfile),
		RecommendedCPUs:     d.recommendedCPUs,
		FoldingSeconds:      int64(d.foldingTime / time.Second),
		ProjectsContributed: d.projects,
		UpdatedAt:           time.Now(),
	}.clone()
	d.status.Store(&st)
	return st.clone()
}

type cmdKind int

const (
	cmdForcePause cmdKind = iota + 1
	cmdForceResume
	cmdSetEcoWatts
	cmdSetPauseProcesses
)

// Command states: a caller that gives up cancels a command the loop has not
// taken, so "busy" never turns into a late surprise.
const (
	cmdQueued int32 = iota
	cmdRunning
	cmdAbandoned
)

type command struct {
	kind     cmdKind
	watts    int
	patterns []string

	state atomic.Int32
	reply chan result
}

type result struct {
	status Status
	err    error
}

// ForcePause pauses folding and restores the default power limit until
// ForceResume, whatever the evaluator decides. A pause the user already made
// in FAH stays the user's.
func (d *Daemon) ForcePause(ctx context.Context) (Status, error) {
	return d.do(ctx, &command{kind: cmdForcePause})
}

// ForceResume clears the manual override and resumes folding now, eco limit
// first. It also resumes a pause the user made in FAH: the explicit request
// is the user's consent, the one exception to "never resume a user pause"
// (CH-10, invariant I1).
//
// Folding is not resumed, and an error is returned, when the evaluator's
// current decision is a pause (ErrGameActive, ErrOnBattery, ErrThermal) or
// when the eco limit cannot be applied (ErrCapFailed). The override is cleared
// in every case, so the daemon resumes its own pause as soon as conditions allow.
func (d *Daemon) ForceResume(ctx context.Context) (Status, error) {
	return d.do(ctx, &command{kind: cmdForceResume})
}

// SetEcoWatts changes the power limit applied while folding. Unlike the
// -eco-watts flag it does not clamp: a value outside the device constraints
// is ErrOutOfRange. The change is not persisted.
func (d *Daemon) SetEcoWatts(ctx context.Context, watts int) (Status, error) {
	return d.do(ctx, &command{kind: cmdSetEcoWatts, watts: watts})
}

// SetPauseProcesses replaces the pause list. An empty list clears it. The
// change is not persisted.
func (d *Daemon) SetPauseProcesses(ctx context.Context, patterns []string) (Status, error) {
	list, err := ValidatePausePatterns(patterns)
	if err != nil {
		return d.Status(), err
	}
	return d.do(ctx, &command{kind: cmdSetPauseProcesses, patterns: list})
}

// ValidatePausePatterns checks a pause list given as separate patterns and
// returns it normalized (see detector.ParsePauseProcesses).
func ValidatePausePatterns(patterns []string) ([]string, error) {
	if len(patterns) > maxPausePatterns {
		return nil, fmt.Errorf("%w: %d patterns, at most %d", ErrInvalidPatterns, len(patterns), maxPausePatterns)
	}
	for _, p := range patterns {
		if strings.Contains(p, ",") {
			return nil, fmt.Errorf("%w: pattern %q contains a comma", ErrInvalidPatterns, p)
		}
	}
	list, err := detector.ParsePauseProcesses(strings.Join(patterns, ","))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPatterns, err)
	}
	return list, nil
}

// do hands c to the loop and waits for the result. It never blocks for long:
// ErrStopping once the daemon stops, ErrBusy when the loop does not take the
// command within cmdTimeout or does not finish it within another cmdTimeout.
func (d *Daemon) do(ctx context.Context, c *command) (Status, error) {
	c.reply = make(chan result, 1)

	select {
	case <-d.stopping:
		return d.Status(), ErrStopping
	default:
	}

	t := time.NewTimer(cmdTimeout)
	defer t.Stop()
	select {
	case d.cmds <- c:
	case <-d.stopping:
		return d.Status(), ErrStopping
	case <-ctx.Done():
		return d.Status(), ctx.Err()
	case <-t.C:
		return d.Status(), ErrBusy
	}

	var cause error
	select {
	case r := <-c.reply:
		return r.status, r.err
	case <-d.stopping:
		cause = ErrStopping
	case <-ctx.Done():
		cause = ctx.Err()
	case <-t.C:
		cause = ErrBusy
	}
	if c.state.CompareAndSwap(cmdQueued, cmdAbandoned) {
		return d.Status(), cause
	}

	// The loop is already running the command: its outcome is real, so wait
	// for it, but not forever.
	t.Reset(cmdTimeout)
	select {
	case r := <-c.reply:
		return r.status, r.err
	case <-t.C:
		return d.Status(), ErrBusy
	}
}

// handle runs one command on the loop goroutine and answers it.
func (d *Daemon) handle(ctx context.Context, c *command) {
	if !c.state.CompareAndSwap(cmdQueued, cmdRunning) {
		return // the caller gave up
	}
	err := d.exec(ctx, c)
	c.reply <- result{status: d.publish(), err: err}
}

func (d *Daemon) exec(ctx context.Context, c *command) error {
	switch c.kind {
	case cmdForcePause:
		return d.forcePause(ctx)
	case cmdForceResume:
		return d.forceResume(ctx)
	case cmdSetEcoWatts:
		return d.setEcoWatts(c.watts)
	case cmdSetPauseProcesses:
		return d.setPauseProcesses(c.patterns)
	default:
		return fmt.Errorf("daemon: unknown command %d", c.kind)
	}
}

// forcePause sets the manual override, restores the default limit and pauses
// FAH unless it is already paused. Power first, as in apply.
func (d *Daemon) forcePause(ctx context.Context) error {
	if d.manual != OverridePause {
		d.log.Info("manual pause requested")
	}
	d.manual = OverridePause
	d.restoreIfCapped()

	if st, err := d.readFAH(ctx); err == nil && st.Paused {
		// Already paused: ours stays ours, the user's stays the user's. Our
		// own pause becomes manual, so a restart keeps it.
		d.pending = opNone
		if d.pausedByUs {
			d.writeMarker()
		}
	} else {
		d.pending = opPause
		d.runPending(ctx)
	}
	if !d.degraded && !(d.haveDecision && isPause(d.lastDecision)) {
		d.mode = ModePausedManual
	}
	if d.pending != opNone {
		return fmt.Errorf("%w: the pause is retried every tick", ErrFAHUnavailable)
	}
	return nil
}

// forceResume clears the manual override and resumes folding now. The eco
// cap is applied before Resume (I2); in Degraded, or without permission,
// power control is off and folding resumes at the default limit.
func (d *Daemon) forceResume(ctx context.Context) error {
	if d.manual != OverrideNone {
		d.log.Info("manual pause cleared")
		d.manual = OverrideNone
		if d.pausedByUs {
			// What is left is an automatic pause: a restart before the
			// resume lands must resume it, not keep it as manual.
			d.writeMarker()
		}
	}
	if !d.degraded {
		if !d.haveDecision {
			d.mode = ModeStarting
		} else {
			d.mode = Mode(d.lastDecision)
			switch d.lastDecision {
			case detector.StatePausedLoad:
				return fmt.Errorf("%w: folding resumes after the cooldown", ErrGameActive)
			case detector.StatePausedBattery:
				return fmt.Errorf("%w: folding resumes when the battery allows it", ErrOnBattery)
			case detector.StatePausedThermal:
				return fmt.Errorf("%w: folding resumes after the machine cools down", ErrThermal)
			}
		}
	}

	// The request outranks the startup hold too.
	d.resumeHold = 0
	if d.powerEnabled && !d.applyCap() {
		return fmt.Errorf("%w: folding stays paused", ErrCapFailed)
	}
	if !d.ensureCPUs(ctx) {
		return fmt.Errorf("%w: could not limit its cpu threads, folding stays paused", ErrFAHUnavailable)
	}

	if st, err := d.readFAH(ctx); err == nil && !st.Paused {
		d.pending = opNone
		if d.pausedByUs {
			d.pausedByUs = false
			if err := d.marker.clear(); err != nil {
				d.log.Warn("could not remove the pause marker", "err", err)
			}
		}
		return nil
	}
	d.pending = opResume
	d.runPending(ctx)
	if d.pending != opNone {
		if !d.pausedByUs {
			// The user's consent covers this attempt only: a retry on a
			// later tick could resume a pause the user makes in between.
			d.pending = opNone
			return fmt.Errorf("%w: folding stays paused, try again", ErrFAHUnavailable)
		}
		return fmt.Errorf("%w: the resume is retried every tick", ErrFAHUnavailable)
	}
	return nil
}

// setEcoWatts validates watts against the constraints read at startup and
// reapplies the cap when it is in effect. On failure the previous value
// stays.
func (d *Daemon) setEcoWatts(watts int) error {
	if !d.powerCapable {
		return fmt.Errorf("%w: this GPU has no power limit control", ErrUnsupported)
	}
	if watts < d.minW || watts > d.maxW {
		return fmt.Errorf("%w: %d W is outside %d-%d W", ErrOutOfRange, watts, d.minW, d.maxW)
	}
	if watts == d.ecoWatts {
		return nil
	}
	if d.capApplied && d.capWanted() {
		if err := d.gpu.SetPowerLimit(watts); err != nil {
			d.log.Warn("could not apply the new eco power limit; keeping the previous one",
				"watts", watts, "previous_w", d.ecoWatts, "err", err)
			return fmt.Errorf("%w: %d W rejected, still %d W: %w", ErrCapFailed, watts, d.ecoWatts, err)
		}
		d.limitUnknown = false
	}
	d.log.Info("eco power limit changed", "from_w", d.ecoWatts, "to_w", watts)
	d.ecoWatts = watts
	return nil
}

// capWanted reports whether the current state calls for the eco cap. While
// it is false a set capApplied only means a restore is still owed, and the
// GPU must not be capped again.
func (d *Daemon) capWanted() bool {
	return d.powerEnabled && !d.degraded && d.manual == OverrideNone &&
		d.haveDecision && !isPause(d.lastDecision) &&
		!(d.resumeHold > 0 && d.pausedByUs)
}

func (d *Daemon) pauseListSupported() bool {
	_, ok := d.collector.(PauseListSetter)
	return ok
}

func (d *Daemon) setPauseProcesses(patterns []string) error {
	setter, ok := d.collector.(PauseListSetter)
	if !ok {
		return fmt.Errorf("%w: this collector has no pause list", ErrUnsupported)
	}
	setter.SetPauseProcesses(patterns)
	d.pauseProcesses = append([]string(nil), patterns...)
	d.log.Info("pause list changed", "patterns", patterns)
	return nil
}

// readFAH reads the FAH status within StatusTimeout and records it for
// Status.
func (d *Daemon) readFAH(ctx context.Context) (fah.Status, error) {
	sctx, cancel := context.WithTimeout(ctx, d.cfg.StatusTimeout)
	defer cancel()
	st, err := d.fah.GetStatus(sctx)
	d.observeFAH(st, err)
	return st, err
}

// refreshFAH reads the FAH status again when the last observation is
// fahRefreshTicks ticks old. It feeds Status and catches FAH folding while
// the daemon believes it holds a pause (see reconcileResume); other decisions
// read FAH themselves when they need it.
func (d *Daemon) refreshFAH(ctx context.Context) {
	if d.fahAge++; d.fahAge < fahRefreshTicks {
		return
	}
	if st, err := d.readFAH(ctx); err == nil && !st.Paused && d.pausedByUs {
		d.reconcileResume(ctx)
	}
}

// reconcileResume handles FAH found unpaused while the daemon owns the pause
// (incident 2026-11-18, 3.3). During a load, battery or thermal pause the
// reason still holds, so FAH is paused again on this pass. Otherwise the
// resume came from outside EcoFold, most likely the user in FAH's own UI, and
// it stays theirs (CH-10): ownership, the manual override and the marker are
// dropped, so the status stops reporting a pause that is not there.
func (d *Daemon) reconcileResume(ctx context.Context) {
	if !d.degraded && d.haveDecision && isPause(d.lastDecision) {
		d.log.Warn("folding@home resumed outside EcoFold during a protective pause; pausing it again", "mode", d.mode)
		d.pending = opPause
		d.runPending(ctx)
		return
	}
	if d.manual == OverridePause {
		d.log.Info("folding@home resumed outside EcoFold; manual pause cleared")
		d.manual = OverrideNone
	} else {
		d.log.Info("folding@home resumed outside EcoFold; the daemon no longer owns a pause")
	}
	d.pausedByUs = false
	d.pending = opNone
	d.resumeHold = 0
	if err := d.marker.clear(); err != nil {
		d.log.Warn("could not remove the pause marker", "err", err)
	}
	if !d.degraded {
		d.mode = ModeStarting
		if d.haveDecision {
			d.mode = Mode(d.lastDecision)
		}
	}
}

// observeFAH records the outcome of a FAH call. On failure the last known
// state is kept.
func (d *Daemon) observeFAH(st fah.Status, err error) {
	d.fahAge = 0
	d.fahStatus.ObservedAt = time.Now()
	d.fahStatus.Reachable = err == nil
	if err == nil {
		d.fahStatus.Paused = st.Paused
		d.fahStatus.State = st.State
		d.fahStatus.ActiveProjectID = st.ActiveProjectID
		if st.CPUs > 0 {
			d.fahStatus.CPUs = st.CPUs
			if d.cpusApplied > 0 && st.CPUs != d.cpusApplied {
				// Changed outside EcoFold: apply the budget again.
				d.cpusApplied = 0
			}
		}
	}
}

// accountFolding adds the time since the previous loop pass to the folding
// total when Folding@home was last seen running a work unit, and records its
// project. A paused, idle or unreachable FAH adds nothing. The step is
// capped, so a stalled loop or a suspended machine is not counted as work.
// Loop goroutine only.
func (d *Daemon) accountFolding(now time.Time) {
	last := d.accountedAt
	d.accountedAt = now
	f := d.fahStatus
	if !f.Reachable || f.Paused || f.State != fah.StateFolding {
		return
	}
	if step := now.Sub(last); !last.IsZero() && step > 0 {
		d.foldingTime += min(step, maxAccountedTicks*d.cfg.Tick)
	}
	if id := f.ActiveProjectID; id > 0 && !slices.Contains(d.projects, id) && len(d.projects) < maxProjects {
		d.projects = append(d.projects, id)
	}
}
