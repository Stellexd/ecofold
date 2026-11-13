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
	// ModeDegraded means game detection is unavailable (per-process GPU
	// utilization unsupported): the default power limit is kept and FAH is
	// left as it is.
	ModeDegraded Mode = "degraded"
)

// collectErrLogEvery rate-limits warnings during a long Collect outage: the
// first error and then one every collectErrLogEvery consecutive errors are
// logged. The same applies to failed FAH actions.
const collectErrLogEvery = 30

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
	// until the evaluator has seen this many snapshots, so a restart while a
	// game is running does not resume folding before the load is detected
	// again. It should match detector.Config.SustainedTicksRequired.
	// Default: 3.
	StartupHoldTicks int

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
		StartupHoldTicks: 3,
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

// Daemon is the control loop. Its state is owned by the loop goroutine; it
// is not safe for concurrent use.
type Daemon struct {
	cfg       Config
	log       *slog.Logger
	gpu       gpu.Provider
	fah       fah.Controller
	collector Collector
	eval      Evaluator
	marker    marker

	// powerCapable is set by start and read-only afterwards, so the
	// fail-safe can read it even if the loop goroutine is still running.
	powerCapable bool

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
	return &Daemon{
		cfg:       cfg,
		log:       cfg.Logger,
		gpu:       deps.GPU,
		fah:       deps.FAH,
		collector: deps.Collector,
		eval:      deps.Evaluator,
		marker:    marker{dir: cfg.RuntimeDir},
		mode:      ModeStarting,
	}, nil
}

// Run initializes the GPU, runs the control loop until ctx is canceled or the
// loop panics, and then runs the fail-safe: restore the default power limit,
// close FAH and shut the GPU down. FAH is never resumed on exit; the pause
// marker is kept so the next start decides from the current load.
func (d *Daemon) Run(ctx context.Context) error {
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
		d.log.Info("stopping")
		select {
		case loopErr = <-done:
		case <-time.After(d.cfg.ShutdownGrace):
			hung = true
			loopErr = errors.New("daemon: control loop did not stop within the grace period")
			d.log.Error("control loop did not stop in time; forcing fail-safe", "grace", d.cfg.ShutdownGrace)
		}
	}
	return errors.Join(loopErr, d.failSafe(hung))
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

	owned, err := d.marker.present()
	if err != nil {
		d.log.Warn("could not read the pause marker; assuming the current pause is not ours", "err", err)
	}
	d.pausedByUs = owned
	if owned {
		d.resumeHold = d.cfg.StartupHoldTicks
	}

	d.log.Info("daemon started",
		"gpu", tel.Name,
		"power_limit_supported", d.powerCapable,
		"eco_watts", d.ecoWatts,
		"paused_by_daemon", d.pausedByUs,
		"tick", d.cfg.Tick)
	return nil
}

// loop ticks until ctx is canceled. A panic is converted into an error so Run
// still runs the fail-safe.
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
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
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
	} else {
		if d.pending == opPause {
			d.pending = opNone // the pause never landed; there is nothing to resume
		}
		if d.applyCap() && d.pausedByUs && !hold {
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
	sctx, cancel := context.WithTimeout(ctx, d.cfg.StatusTimeout)
	st, err := d.fah.GetStatus(sctx)
	cancel()
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
	if err := call(actx); err != nil {
		d.fahErrs++
		if d.fahErrs == 1 || d.fahErrs%collectErrLogEvery == 0 {
			d.log.Warn("fah action failed; retrying next tick", "op", op, "err", err, "consecutive", d.fahErrs)
		}
		return
	}
	d.fahErrs = 0
	d.pending = opNone

	if op == "pause" {
		d.pausedByUs = true
		if err := d.marker.set(); err != nil {
			d.log.Warn("could not write the pause marker", "err", err)
		}
		d.log.Info("folding paused")
		return
	}
	d.pausedByUs = false
	if err := d.marker.clear(); err != nil {
		d.log.Warn("could not remove the pause marker", "err", err)
	}
	d.log.Info("folding resumed")
}

// failSafe restores the default power limit within RestoreTimeout and
// releases the controllers. When the loop or the restore is stuck inside a
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
	return s == detector.StatePausedLoad || s == detector.StatePausedBattery
}
