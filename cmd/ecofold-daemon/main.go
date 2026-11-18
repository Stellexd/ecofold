// Command ecofold-daemon runs EcoFold's control loop. On Linux it is the only
// binary allowed to hold CAP_SYS_ADMIN, which NVML needs to change the GPU
// power limit. On macOS it runs unprivileged as Smart Pause & Battery Guard.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
	"github.com/Stellexd/ecofold/internal/ipc"
)

const (
	minTick = time.Second
	maxTick = 5 * time.Second

	// pauseFAHTimeout bounds -pause-fah, including FAH's confirmation.
	pauseFAHTimeout = 3 * time.Second
)

func main() {
	os.Exit(run())
}

// run returns the process exit code. It never calls os.Exit itself, so the
// daemon's deferred fail-safe always runs.
func run() int {
	def := daemon.DefaultConfig()
	// No fallback path: without a default the socket stays off unless
	// -socket-path is given.
	defSocket, sockErr := ipc.DefaultPath()
	var (
		fahEndpoint = flag.String("fah-endpoint", fah.DefaultEndpoint, "Folding@home v8 WebSocket API (loopback only)")
		fahMock     = flag.Bool("fah-mock", false, "use an in-memory FAH controller (development without Folding@home)")
		ecoWatts    = flag.Int("eco-watts", def.EcoWatts, "GPU power limit in watts while folding (clamped to the device constraints)")
		tick        = flag.Duration("tick", def.Tick, "control loop period (1s-5s)")
		runtimeDir  = flag.String("runtime-dir", defaultRuntimeDir(), "directory for the pause-ownership marker")
		restoreOnly = flag.Bool("restore-power-limit", false, "restore the default GPU power limit and exit (for systemd ExecStopPost)")
		pauseOnly   = flag.Bool("pause-fah", false, "pause Folding@home and exit (emergency stop; works without a running daemon)")
		verbose     = flag.Bool("debug", false, "enable debug logging")
		pauseList   = flag.String("pause-processes", "", "comma-separated process names that stop folding while they run, e.g. \"blender,obs\" (trailing * is a prefix match)")
		socketPath  = flag.String("socket-path", defSocket, "Unix socket for the GUI; empty disables IPC")
		socketGroup = socketGroupFlag()
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *restoreOnly {
		return restorePowerLimit(logger)
	}
	if *pauseOnly {
		c, err := fah.NewClientV8(*fahEndpoint)
		if err != nil {
			logger.Error("invalid -fah-endpoint", "err", err)
			return 2
		}
		return pauseFAH(logger, c)
	}

	if *tick < minTick || *tick > maxTick {
		logger.Error("invalid -tick", "tick", *tick, "min", minTick, "max", maxTick)
		return 2
	}
	if *ecoWatts <= 0 {
		logger.Error("invalid -eco-watts", "watts", *ecoWatts)
		return 2
	}
	pause, err := detector.ParsePauseProcesses(*pauseList)
	if err != nil {
		logger.Error("invalid -pause-processes", "err", err)
		return 2
	}
	if len(pause) > 0 && !pauseListSupported {
		logger.Warn("-pause-processes is not supported on this platform; ignoring it", "patterns", pause)
		pause = nil
	}
	if sockErr != nil && *socketPath == "" {
		logger.Warn("no default socket path", "err", sockErr)
	}

	var ctrl fah.Controller
	if *fahMock {
		m := fah.NewMockController(0)
		// Report a cpus value, as a real client does, so the macOS CPU
		// budget can be recorded and applied.
		_ = m.SetCPUs(context.Background(), runtime.NumCPU())
		ctrl = m
	} else {
		c, err := fah.NewClientV8(*fahEndpoint)
		if err != nil {
			logger.Error("invalid -fah-endpoint", "err", err)
			return 2
		}
		ctrl = c
	}

	provider := gpu.NewProvider()
	detCfg := detector.DefaultConfig()
	cfg := def
	cfg.Tick = *tick
	cfg.EcoWatts = *ecoWatts
	cfg.RuntimeDir = *runtimeDir
	cfg.StartupHoldTicks = detCfg.CooldownTicksRequired
	cfg.PauseProcesses = pause
	cfg.Logger = logger
	if len(pause) > 0 {
		logger.Info("pause list active", "patterns", pause)
	}

	d, err := daemon.New(cfg, daemon.Deps{
		GPU:       provider,
		FAH:       ctrl,
		Collector: newCollector(provider),
		Evaluator: detector.NewEvaluator(detCfg),
	})
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		_ = ctrl.Close()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, logger, d, *socketPath, *socketGroup); err != nil {
		if errors.Is(err, ipc.ErrAlreadyRunning) {
			// Run never started, so nothing else was touched.
			_ = ctrl.Close()
			logger.Error("refusing to start a second daemon", "socket", *socketPath, "err", err)
			return 1
		}
		logger.Error("daemon stopped with errors", "err", err)
		return 1
	}
	logger.Info("daemon stopped")
	return 0
}

// pauseFAH is the emergency stop: it pauses Folding@home within
// pauseFAHTimeout and closes ctrl. It opens no socket, starts no loop and
// never touches the GPU, so it works whether or not a daemon is running. A
// running daemon sees the pause as the user's and does not resume it (CH-10).
func pauseFAH(logger *slog.Logger, ctrl fah.Controller) int {
	defer func() { _ = ctrl.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), pauseFAHTimeout)
	defer cancel()
	if err := ctrl.Pause(ctx); err != nil {
		logger.Error("could not pause Folding@home", "err", err)
		return 1
	}
	logger.Info("Folding@home paused")
	return 0
}

// restorePowerLimit is the one-shot fail-safe used by systemd ExecStopPost:
// it runs in a fresh process with its own NVML handle, so it works even when
// the daemon was killed or hung inside a driver call.
func restorePowerLimit(logger *slog.Logger) int {
	p := gpu.NewProvider()
	if err := p.Init(); err != nil {
		logger.Error("gpu init failed", "err", err)
		return 1
	}
	defer func() { _ = p.Shutdown() }()

	tel, err := p.GetTelemetry()
	if err != nil {
		logger.Error("gpu telemetry failed", "err", err)
		return 1
	}
	if !tel.SupportsPowerLimit {
		logger.Info("power limit not supported on this device; nothing to restore")
		return 0
	}
	if err := p.RestoreDefaultPowerLimit(); err != nil {
		logger.Error("restore failed", "err", err)
		return 1
	}
	logger.Info("default power limit restored", "gpu", tel.Name, "watts", tel.DefaultPowerLimitW)
	return 0
}
