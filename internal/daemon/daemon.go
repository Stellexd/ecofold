// Package daemon runs EcoFold's control loop: every tick it collects system
// telemetry, evaluates the operating state and applies it to the GPU power
// limit and the Folding@home client.
//
// The package is platform-agnostic. On Linux with NVML it caps the GPU to the
// eco limit while folding and restores the default limit when a game or low
// battery pauses folding. Where the GPU does not support power limits (macOS,
// the Linux stub) it acts only as Smart Pause & Battery Guard on FAH.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// Collector produces one telemetry snapshot per tick. detector.LinuxCollector
// and detector.DarwinCollector satisfy it.
type Collector interface {
	Collect(ctx context.Context) (detector.SystemSnapshot, error)
}

// Evaluator turns snapshots into operating states. *detector.Evaluator
// satisfies it.
type Evaluator interface {
	Evaluate(detector.SystemSnapshot) detector.OperatingState
}

// Mode is the daemon's externally visible state.
type Mode string

const (
	ModeStarting      Mode = "starting"
	ModeFolding       Mode = Mode(detector.StateFolding)
	ModeFoldingEco    Mode = Mode(detector.StateFoldingEco)
	ModePausedLoad    Mode = Mode(detector.StatePausedLoad)
	ModePausedBattery Mode = Mode(detector.StatePausedBattery)
	ModePausedThermal Mode = Mode(detector.StatePausedThermal)
	// ModeDegraded means game detection is unavailable (per-process GPU
	// utilization unsupported): the default power limit is kept and FAH is
	// left as it is.
	ModeDegraded Mode = "degraded"
	// ModePausedManual means a ForcePause is in effect and the evaluator
	// would otherwise fold.
	ModePausedManual Mode = "paused_manual"
)

// collectErrLogEvery rate-limits warnings during a long Collect outage: the
// first error and then one every collectErrLogEvery consecutive errors are
// logged. The same applies to failed FAH actions.
const collectErrLogEvery = 30

// fahRefreshTicks is how many ticks a FAH observation stays current before
// the loop reads the status again (10 s at the default tick).
const fahRefreshTicks = 5

const (
	// maxAccountedTicks caps, in ticks, the time one loop pass adds to the
	// folding total.
	maxAccountedTicks = 3
	// maxProjects caps Status.ProjectsContributed.
	maxProjects = 256
)

// Config holds the daemon's tunables. Zero durations are replaced by
// DefaultConfig values.
type Config struct {
	// Tick is the control loop period. Default: 2 s.
	Tick time.Duration
	// EcoWatts is the power limit applied while folding, clamped to the
	// device constraints. Default: 123 W.
	EcoWatts int
	// RuntimeDir holds the pause-ownership marker. Required.
	RuntimeDir string
	// StartupHoldTicks delays resuming a pause inherited through the marker
	// until the evaluator has seen this many snapshots, and keeps the default
	// power limit meanwhile. A restart loses the evaluator's cooldown, so the
	// hold replaces it: a game still running (even idle in a menu) is neither
	// capped nor has folding resumed under it before a full cooldown. It
	// should match detector.Config.CooldownTicksRequired. Default: 60.
	StartupHoldTicks int
	// PauseProcesses is the initial pause list, already validated with
	// detector.ParsePauseProcesses. It is handed to the collector at startup
	// when the collector is a PauseListSetter.
	PauseProcesses []string

	// CollectTimeout bounds one Collect call. Default: 1.5 s.
	CollectTimeout time.Duration
	// StatusTimeout bounds fah.Controller.GetStatus. Default: 500 ms.
	StatusTimeout time.Duration
	// ActionTimeout bounds fah.Controller.Pause and Resume, including the
	// confirming snapshot. Default: 1.5 s.
	ActionTimeout time.Duration
	// RestoreTimeout bounds the power-limit restore on exit. Default: 2 s.
	RestoreTimeout time.Duration
	// ShutdownGrace is how long Run waits for an in-flight tick after the
	// context is canceled. Default: 3 s.
	ShutdownGrace time.Duration

	// Logger receives the daemon's logs. Default: slog.Default().
	Logger *slog.Logger
}

// DefaultConfig returns the documented defaults. RuntimeDir is left empty
// because it depends on the platform.
func DefaultConfig() Config {
	return Config{
		Tick:             2 * time.Second,
		EcoWatts:         123,
		StartupHoldTicks: 60,
		CollectTimeout:   1500 * time.Millisecond,
		StatusTimeout:    500 * time.Millisecond,
		ActionTimeout:    1500 * time.Millisecond,
		RestoreTimeout:   2 * time.Second,
		ShutdownGrace:    3 * time.Second,
	}
}

// Deps are the daemon's collaborators. All are required.
type Deps struct {
	GPU       gpu.Provider
	FAH       fah.Controller
	Collector Collector
	Evaluator Evaluator
}

type fahOp int

const (
	opNone fahOp = iota
	opPause
	opResume
)

// Daemon is the control loop. Its state is owned by the loop goroutine and
// is not locked. Other goroutines may only call Status and the command
// methods (ForcePause, ForceResume, SetEcoWatts, SetPauseProcesses), which
// read a published snapshot or hand a command to the loop.
type Daemon struct {
	cfg       Config
	log       *slog.Logger
	gpu       gpu.Provider
	fah       fah.Controller
	collector Collector
	eval      Evaluator
	marker    marker

	// cmds carries commands to the loop; stopping is closed when Run begins
	// to shut down; status is the snapshot other goroutines read.
	cmds     chan *command
	stopping chan struct{}
	stopOnce sync.Once
	status   atomic.Pointer[Status]

	// powerCapable is set by start and read-only afterwards, so the
	// fail-safe can read it even if the loop goroutine is still running.
	powerCapable bool
	// Power limit constraints read by start.
	minW, maxW, defaultW int

	mode         Mode
	lastDecision detector.OperatingState
	haveDecision bool
	powerEnabled bool // false after NVML_ERROR_NO_PERMISSION or in Degraded
	ecoWatts     int
	capApplied   bool // the eco cap is (or may be) in effect
	limitUnknown bool // a restore failed, so the real limit is unknown
	pausedByUs   bool // the current FAH pause is ours (mirrored by the marker)
	pending      fahOp
	degraded     bool
	collectErrs  int
	fahErrs      int
	resumeHold   int // evaluated ticks left before an inherited pause may resume
	manual       Override

	// Last known values, kept only for Status.
	pauseProcesses []string
	powerLimitW    int
	gpuStatus      GPUStatus
	battery        BatteryStatus
	fahStatus      FAHStatus
	fahAge         int    // ticks since fahStatus was observed
	gpuName        string // from the collector; published for a simulated GPU
	gpuUtilPct     int
	gpuInUseMemMiB int
	sessions       []string
	pauseMatches   []string

	// macProfile and recommendedCPUs come from the last snapshot (macOS).
	macProfile      detector.MacThermalProfile
	recommendedCPUs int

	// FAH CPU threads (macOS), handled like the GPU power limit: the user's
	// value is recorded before the first change and restored on exit.
	// cpuLimiter is nil when the FAH controller cannot change cpus.
	cpuLimiter   fah.CPULimiter
	originalCPUs int  // FAH's cpus before the daemon's first change; 0 until read
	cpusChanged  bool // the daemon has sent SetCPUs (it may have landed)
	cpusApplied  int  // the value FAH confirmed last; 0 when unknown
	cpuErrs      int

	// Contribution of this run, for Status. See accountFolding.
	foldingTime time.Duration
	projects    []int
	accountedAt time.Time // previous accountFolding call; zero before the first
}

// New validates cfg and deps and returns a Daemon. It does not touch the
// hardware; Run does.
func New(cfg Config, deps Deps) (*Daemon, error) {
	if deps.GPU == nil || deps.FAH == nil || deps.Collector == nil || deps.Evaluator == nil {
		return nil, errors.New("daemon: missing dependency")
	}
	if cfg.RuntimeDir == "" {
		return nil, errors.New("daemon: RuntimeDir is required")
	}
	def := DefaultConfig()
	for _, f := range []struct{ v, d *time.Duration }{
		{&cfg.Tick, &def.Tick},
		{&cfg.CollectTimeout, &def.CollectTimeout},
		{&cfg.StatusTimeout, &def.StatusTimeout},
		{&cfg.ActionTimeout, &def.ActionTimeout},
		{&cfg.RestoreTimeout, &def.RestoreTimeout},
		{&cfg.ShutdownGrace, &def.ShutdownGrace},
	} {
		if *f.v <= 0 {
			*f.v = *f.d
		}
	}
	if cfg.EcoWatts <= 0 {
		cfg.EcoWatts = def.EcoWatts
	}
	if cfg.StartupHoldTicks <= 0 {
		cfg.StartupHoldTicks = def.StartupHoldTicks
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	limiter, _ := deps.FAH.(fah.CPULimiter)
	return &Daemon{
		cpuLimiter: limiter,
		cfg:        cfg,
		log:        cfg.Logger,
		gpu:        deps.GPU,
		fah:        deps.FAH,
		collector:  deps.Collector,
		eval:       deps.Evaluator,
		marker:     marker{dir: cfg.RuntimeDir},
		cmds:       make(chan *command, cmdQueue),
		stopping:   make(chan struct{}),
		mode:       ModeStarting,
		manual:     OverrideNone,
		fahStatus:  FAHStatus{State: fah.StateUnknown},
		fahAge:     fahRefreshTicks, // read FAH on the first loop pass
	}, nil
}

// Run initializes the GPU, runs the control loop until ctx is canceled or the
// loop panics, and then runs the fail-safe: restore the default power limit,
// close FAH and shut the GPU down. FAH is never resumed on exit; the pause
// marker is kept so the next start decides from the current load.
func (d *Daemon) Run(ctx context.Context) error {
	// Commands fail with ErrStopping from the moment shutdown begins, and
	// after any return.
	defer d.markStopping()
	if err := d.start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- d.loop(ctx) }()

	var loopErr error
	hung := false
	select {
	case loopErr = <-done:
	case <-ctx.Done():
		d.markStopping()
		d.log.Info("stopping")
		select {
		case loopErr = <-done:
		case <-time.After(d.cfg.ShutdownGrace):
			hung = true
			loopErr = errors.New("daemon: control loop did not stop within the grace period")
			d.log.Error("control loop did not stop in time; forcing fail-safe", "grace", d.cfg.ShutdownGrace)
		}
	}
	d.markStopping()
	return errors.Join(loopErr, d.failSafe(hung))
}

func (d *Daemon) markStopping() {
	d.stopOnce.Do(func() { close(d.stopping) })
}

// start initializes the GPU and loads persisted state.
func (d *Daemon) start() error {
	if err := d.gpu.Init(); err != nil {
		return fmt.Errorf("daemon: gpu init: %w", err)
	}
	tel, err := d.gpu.GetTelemetry()
	if err != nil {
		_ = d.gpu.Shutdown()
		return fmt.Errorf("daemon: gpu telemetry: %w", err)
	}

	d.powerCapable = tel.SupportsPowerLimit
	if d.powerCapable && (tel.MaxPowerLimitW <= 0 || tel.MinPowerLimitW > tel.MaxPowerLimitW) {
		d.log.Warn("invalid power limit constraints; running pause-only",
			"min_w", tel.MinPowerLimitW, "max_w", tel.MaxPowerLimitW)
		d.powerCapable = false
	}
	d.powerEnabled = d.powerCapable
	d.minW, d.maxW, d.defaultW = tel.MinPowerLimitW, tel.MaxPowerLimitW, tel.DefaultPowerLimitW
	if d.powerCapable {
		d.ecoWatts = min(max(d.cfg.EcoWatts, tel.MinPowerLimitW), tel.MaxPowerLimitW)
		if d.ecoWatts != d.cfg.EcoWatts {
			d.log.Warn("eco power limit clamped to device constraints",
				"requested_w", d.cfg.EcoWatts, "applied_w", d.ecoWatts)
		}
		// Normalize to the default limit: a cap left by a crashed instance
		// must not survive. If this fails the limit is unknown, so treat it
		// as capped and restore again on the first pause.
		switch err := d.gpu.RestoreDefaultPowerLimit(); {
		case errors.Is(err, gpu.ErrNoPermission):
			// Nothing was changed and nothing can be: skip the exit restore too.
			d.powerCapable, d.powerEnabled = false, false
			d.log.Error("no permission to change the GPU power limit; running pause-only", "err", err)
		case err != nil:
			d.log.Warn("could not restore the default power limit at startup", "err", err)
			d.capApplied, d.limitUnknown = true, true
		}
	}

	origin, owned, err := d.marker.read()
	if err != nil {
		d.log.Warn("could not read the pause marker; assuming the current pause is not ours", "err", err)
	}
	d.pausedByUs = owned
	switch {
	case owned && origin == originManual:
		// The user's ForcePause outlives the restart: no startup hold, and
		// nothing resumes until ForceResume.
		d.manual = OverridePause
		d.mode = ModePausedManual
	case owned:
		d.resumeHold = d.cfg.StartupHoldTicks
	}

	if len(d.cfg.PauseProcesses) > 0 {
		if setter, ok := d.collector.(PauseListSetter); ok {
			d.pauseProcesses = append([]string(nil), d.cfg.PauseProcesses...)
			setter.SetPauseProcesses(d.pauseProcesses)
		} else {
			d.log.Warn("this collector has no pause list; ignoring it", "patterns", d.cfg.PauseProcesses)
		}
	}
	d.publish()

	d.log.Info("daemon started",
		"gpu", tel.Name,
		"power_limit_supported", d.powerCapable,
		"eco_watts", d.ecoWatts,
		"paused_by_daemon", d.pausedByUs,
		"manual_pause", d.manual == OverridePause,
		"tick", d.cfg.Tick)
	return nil
}

// loop ticks until ctx is canceled and runs commands between ticks. A panic
// is converted into an error so Run still runs the fail-safe.
func (d *Daemon) loop(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("daemon: panic in control loop: %v", r)
			d.log.Error("panic in control loop", "panic", r, "stack", string(debug.Stack()))
		}
	}()

	t := time.NewTicker(d.cfg.Tick)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		d.tick(ctx)
		d.refreshFAH(ctx)
		d.accountFolding(time.Now())
		d.publish()
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				break wait
			case c := <-d.cmds:
				if ctx.Err() != nil {
					return nil
				}
				d.handle(ctx, c)
			}
		}
	}
}

// tick runs one collect → evaluate → apply cycle.
func (d *Daemon) tick(ctx context.Context) {
	if d.degraded {
		d.restoreIfCapped()
		d.runPending(ctx)
		return
	}

	cctx, cancel := context.WithTimeout(ctx, d.cfg.CollectTimeout)
	snap, err := d.collector.Collect(cctx)
	cancel()
	switch {
	case errors.Is(err, detector.ErrPerProcessUtilUnsupported):
		d.enterDegraded()
		d.runPending(ctx)
		return
	case err != nil:
		// Keep the last decision: never feed "no load" to the evaluator, and
		// leave its streaks and cooldown untouched.
		d.collectErrs++
		if d.collectErrs == 1 || d.collectErrs%collectErrLogEvery == 0 {
			d.log.Warn("collect failed; keeping the last decision", "err", err, "consecutive", d.collectErrs)
		}
		d.runPending(ctx)
		return
	}
	if d.collectErrs > 0 {
		d.log.Info("collect recovered", "after_errors", d.collectErrs)
		d.collectErrs = 0
	}
	d.battery = BatteryStatus{
		Present:  snap.HasBattery,
		OnAC:     snap.OnAC,
		Charging: snap.IsCharging,
		Pct:      snap.BatteryPct,
	}
	d.sessions, d.pauseMatches = snap.LiveGameSessions, snap.UserPauseMatches
	d.gpuName = snap.GPUModel
	if d.gpuName == "" {
		d.gpuName = snap.GPUClass
	}
	d.gpuUtilPct, d.gpuInUseMemMiB = snap.GPUDeviceUtilPct, snap.GPUInUseMemMiB
	d.macProfile, d.recommendedCPUs = snap.MacProfile, snap.RecommendedCPUs
	if len(snap.GPUApps) > 0 && d.log.Enabled(ctx, slog.LevelDebug) {
		d.log.Debug("gpu apps", "device_util_pct", snap.GPUDeviceUtilPct, "apps", formatGPUApps(snap.GPUApps))
	}

	d.apply(ctx, d.eval.Evaluate(snap))
}

// apply drives the GPU and FAH towards decision. Power is changed first in
// both directions: on pause the game gets the default limit even if FAH is
// slow or down; on resume folding only restarts once the eco cap is in place.
func (d *Daemon) apply(ctx context.Context, decision detector.OperatingState) {
	prevPause := d.haveDecision && isPause(d.lastDecision)
	hold := d.resumeHold > 0
	if hold {
		d.resumeHold--
	}
	if !d.haveDecision || decision != d.lastDecision {
		d.log.Info("state change", "from", d.mode, "to", decision)
	}
	d.lastDecision, d.haveDecision = decision, true
	d.mode = Mode(decision)

	if isPause(decision) {
		d.restoreIfCapped()
		if !prevPause {
			d.claimPause(ctx)
		}
	} else if d.manual == OverridePause {
		// The user's pause outranks the evaluator: nothing resumes, the GPU
		// keeps its default limit and a pause FAH has not confirmed yet stays
		// pending.
		d.mode = ModePausedManual
		d.restoreIfCapped()
	} else {
		if d.pending == opPause {
			d.pending = opNone // the pause never landed; there is nothing to resume
		}
		switch {
		case hold && d.pausedByUs:
			// The inherited pause may still belong to a running game: keep
			// the default limit until the hold ends.
			d.restoreIfCapped()
		case d.applyCap() && d.ensureCPUs(ctx) && d.pausedByUs:
			d.pending = opResume
		}
	}
	d.runPending(ctx)
}

// claimPause decides whether to pause FAH on a transition into a pause. A
// pause that is already in place and not ours belongs to the user and is left
// alone (CH-10). Any other status, including idle, unknown or unreadable,
// gets a Pause (CH2-H1).
func (d *Daemon) claimPause(ctx context.Context) {
	st, err := d.readFAH(ctx)
	if err == nil && st.Paused {
		d.pending = opNone
		if !d.pausedByUs {
			d.log.Info("folding already paused by the user; it will not be resumed automatically")
		}
		return
	}
	if err != nil {
		d.log.Warn("fah status unavailable; pausing anyway", "err", err)
	}
	d.pending = opPause
}

// applyCap applies the eco power limit if needed and reports whether folding
// may resume.
func (d *Daemon) applyCap() bool {
	if !d.powerEnabled || (d.capApplied && !d.limitUnknown) {
		return true
	}
	if err := d.gpu.SetPowerLimit(d.ecoWatts); err != nil {
		if errors.Is(err, gpu.ErrNoPermission) {
			d.powerEnabled = false
			d.log.Error("no permission to change the GPU power limit; running pause-only", "err", err)
			return true
		}
		d.log.Warn("could not apply the eco power limit; folding stays paused until it succeeds",
			"watts", d.ecoWatts, "err", err)
		return false
	}
	d.capApplied, d.limitUnknown = true, false
	d.log.Info("eco power limit applied", "watts", d.ecoWatts)
	return true
}

// ensureCPUs sets FAH's cpus to the recommended budget when FAH is not
// known to use it already, and reports whether folding may resume. It runs
// before every resume (like the eco cap, I2) and on every folding tick, so a
// budget that changes with the power source or the thermal level reaches FAH
// while it folds. The user's value is read and kept before the first change.
// Without a budget (Linux) or a limiter it does nothing.
func (d *Daemon) ensureCPUs(ctx context.Context) bool {
	target := d.recommendedCPUs
	if target <= 0 || d.cpuLimiter == nil || d.cpusApplied == target {
		return true
	}
	if d.originalCPUs == 0 {
		st, err := d.readFAH(ctx)
		if err != nil || st.CPUs <= 0 {
			d.cpuFailed("could not read Folding@home's cpus; folding stays paused", err)
			return false
		}
		d.originalCPUs = st.CPUs
		if st.CPUs == target {
			d.cpusApplied = target
			return true
		}
	}
	actx, cancel := context.WithTimeout(ctx, d.cfg.ActionTimeout)
	defer cancel()
	d.cpusChanged = true
	if err := d.cpuLimiter.SetCPUs(actx, target); err != nil {
		d.cpusApplied = 0
		d.cpuFailed("could not limit Folding@home's cpus; folding stays paused", err)
		return false
	}
	d.cpuErrs = 0
	d.cpusApplied = target
	d.fahStatus.CPUs = target
	d.log.Info("folding@home cpu threads set", "cpus", target, "user_cpus", d.originalCPUs, "profile", d.macProfile)
	return true
}

func (d *Daemon) cpuFailed(msg string, err error) {
	d.cpuErrs++
	if d.cpuErrs == 1 || d.cpuErrs%collectErrLogEvery == 0 {
		d.log.Warn(msg, "cpus", d.recommendedCPUs, "err", err, "consecutive", d.cpuErrs)
	}
}

// restoreCPUs puts back the cpus FAH had before the daemon changed them,
// within RestoreTimeout. On a fanless Mac this can bring back the load that
// overheated it; the log says so.
func (d *Daemon) restoreCPUs() error {
	if !d.cpusChanged || d.originalCPUs <= 0 || d.cpuLimiter == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.RestoreTimeout)
	defer cancel()
	if err := d.cpuLimiter.SetCPUs(ctx, d.originalCPUs); err != nil {
		d.log.Error("could not restore Folding@home's cpus on exit", "cpus", d.originalCPUs, "err", err)
		return fmt.Errorf("daemon: restore fah cpus: %w", err)
	}
	d.log.Info("folding@home cpus restored on exit", "cpus", d.originalCPUs)
	if d.macProfile == detector.ProfileFanless && d.originalCPUs > 1 && !d.fahStatus.Paused {
		d.log.Warn("folding@home keeps folding with the restored cpus on a fanless Mac; "+
			"pause it with ecofold-daemon -pause-fah if EcoFold stays off", "cpus", d.originalCPUs)
	}
	return nil
}

// restoreIfCapped restores the default power limit when the eco cap may be in
// effect. A failure is retried on the next call.
func (d *Daemon) restoreIfCapped() {
	if !d.capApplied {
		return
	}
	if err := d.gpu.RestoreDefaultPowerLimit(); err != nil {
		d.log.Warn("could not restore the default power limit; retrying next tick", "err", err)
		return
	}
	d.capApplied, d.limitUnknown = false, false
	d.log.Info("default power limit restored")
}

// enterDegraded latches Degraded mode: game detection is off, the default
// power limit is restored and FAH is no longer driven automatically. A
// pending action the daemon already started is still completed.
func (d *Daemon) enterDegraded() {
	d.degraded = true
	d.powerEnabled = false
	d.mode = ModeDegraded
	d.log.Warn("per-process GPU utilization unsupported by the driver: game detection disabled, " +
		"power limit restored to default, folding left as is")
	d.restoreIfCapped()
}

// runPending sends the pending FAH action, if any. Ownership changes only
// after FAH confirms the action; a failure keeps the action pending for the
// next tick.
func (d *Daemon) runPending(ctx context.Context) {
	if d.pending == opNone {
		return
	}
	actx, cancel := context.WithTimeout(ctx, d.cfg.ActionTimeout)
	defer cancel()

	op, call := "pause", d.fah.Pause
	if d.pending == opResume {
		op, call = "resume", d.fah.Resume
	}
	err := call(actx)
	if op == "pause" {
		d.observeFAH(fah.Status{Paused: true, State: fah.StatePaused}, err)
	} else {
		d.observeFAH(fah.Status{State: fah.StateUnknown}, err)
	}
	if err != nil {
		d.fahErrs++
		if d.fahErrs == 1 || d.fahErrs%collectErrLogEvery == 0 {
			d.log.Warn("fah action failed; retrying next tick", "op", op, "err", err, "consecutive", d.fahErrs)
		}
		return
	}
	d.fahErrs = 0
	d.pending = opNone

	if op == "resume" {
		// Resume does not say what FAH is doing now (folding or idle): the
		// loop reads it on its next pass.
		d.fahAge = fahRefreshTicks
	}
	if op == "pause" {
		d.pausedByUs = true
		d.writeMarker()
		d.log.Info("folding paused")
		return
	}
	d.pausedByUs = false
	if err := d.marker.clear(); err != nil {
		d.log.Warn("could not remove the pause marker", "err", err)
	}
	d.log.Info("folding resumed")
}

// writeMarker records the daemon's pause with its origin: manual while the
// override is set, so a manual pause is never downgraded to auto.
func (d *Daemon) writeMarker() {
	origin := originAuto
	if d.manual == OverridePause {
		origin = originManual
	}
	if err := d.marker.set(origin); err != nil {
		d.log.Warn("could not write the pause marker", "err", err)
	}
}

// failSafe restores the default power limit and FAH's cpus within
// RestoreTimeout each and releases the controllers. When the loop or the restore is stuck inside a
// GPU call, Shutdown is skipped because it would block on the same call.
func (d *Daemon) failSafe(loopHung bool) error {
	var errs []error
	restoreStuck := false
	if d.powerCapable {
		res := make(chan error, 1)
		go func() { res <- d.gpu.RestoreDefaultPowerLimit() }()
		select {
		case err := <-res:
			if err != nil {
				d.log.Error("could not restore the default power limit on exit", "err", err)
				errs = append(errs, fmt.Errorf("daemon: restore power limit: %w", err))
			} else {
				d.log.Info("default power limit restored on exit")
			}
		case <-time.After(d.cfg.RestoreTimeout):
			restoreStuck = true
			d.log.Error("power limit restore timed out on exit", "timeout", d.cfg.RestoreTimeout)
			errs = append(errs, errors.New("daemon: restore power limit: timed out"))
		}
	}
	// The cpus fields belong to the loop goroutine: with the loop stuck they
	// cannot be read safely, and the restore is skipped.
	if loopHung {
		if d.cpuLimiter != nil {
			d.log.Error("control loop stuck; folding@home's cpus were not restored")
		}
	} else if err := d.restoreCPUs(); err != nil {
		errs = append(errs, err)
	}
	if err := d.fah.Close(); err != nil {
		errs = append(errs, fmt.Errorf("daemon: close fah: %w", err))
	}
	if !loopHung && !restoreStuck {
		if err := d.gpu.Shutdown(); err != nil {
			errs = append(errs, fmt.Errorf("daemon: gpu shutdown: %w", err))
		}
	}
	return errors.Join(errs...)
}

func isPause(s detector.OperatingState) bool {
	return s == detector.StatePausedLoad || s == detector.StatePausedBattery || s == detector.StatePausedThermal
}

// formatGPUApps renders per-app GPU time for the -debug dump, for example
// "Safari[812]=31% WindowServer[604]=4%".
func formatGPUApps(apps []detector.GPUAppUsage) string {
	var b strings.Builder
	for i, a := range apps {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s[%d]=%d%%", a.Name, a.PID, a.GPUTimePct)
	}
	return b.String()
}
