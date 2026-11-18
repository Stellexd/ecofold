package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/ipc"
)

// ipcShutdownGrace is how long shutdown waits for in-flight requests before
// the control loop stops.
const ipcShutdownGrace = time.Second

// serve runs the daemon with its IPC socket until ctx is canceled and
// returns Run's error.
//
// Shutdown order: the socket closes first (no new commands, in-flight ones
// get up to ipcShutdownGrace), then the loop stops, then Run's fail-safe
// restores the power limit. The socket also closes when Run ends on its own.
//
// A socket that cannot be created is logged and the daemon runs without IPC:
// capping and restoring the GPU must not depend on the GUI's channel.
func serve(ctx context.Context, logger *slog.Logger, d *daemon.Daemon, socketPath, socketGroup string) error {
	shutdown := startIPC(logger, d, socketPath, socketGroup)

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown()
			cancelRun()
		case <-runDone:
		}
	}()

	err := d.Run(runCtx)
	close(runDone)
	shutdown()
	return err
}

// startIPC opens the socket and returns the function that closes it. On
// failure, or with an empty path, it returns a no-op.
func startIPC(logger *slog.Logger, d *daemon.Daemon, path, group string) (shutdown func()) {
	if path == "" {
		logger.Warn("ipc disabled: no socket path")
		return func() {}
	}
	srv, err := ipc.Listen(ipc.Config{Path: path, Group: group, Handler: handler{d: d}, Logger: logger})
	if err != nil {
		logger.Error("ipc unavailable; the daemon keeps running without it", "socket", path, "err", err)
		return func() {}
	}
	logger.Info("ipc listening", "socket", path)
	go func() { _ = srv.Serve() }()

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), ipcShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Warn("ipc shutdown did not finish in time", "err", err)
		}
	}
}

// handler adapts the daemon to ipc.Handler. Status reads the published
// snapshot; every other command goes through the loop's command channel.
type handler struct {
	d *daemon.Daemon
}

func (h handler) Handle(ctx context.Context, req ipc.Request) ipc.Response {
	switch req.Cmd {
	case ipc.CmdStatus:
		if err := ipc.DecodeArgs(req.Args, &struct{}{}); err != nil {
			return ipc.Fail(ipc.CodeBadRequest, "status takes no args")
		}
		return ipc.OK(h.d.Status())

	case ipc.CmdForcePause:
		if err := ipc.DecodeArgs(req.Args, &struct{}{}); err != nil {
			return ipc.Fail(ipc.CodeBadRequest, "force_pause takes no args")
		}
		return reply(h.d.ForcePause(ctx))

	case ipc.CmdForceResume:
		if err := ipc.DecodeArgs(req.Args, &struct{}{}); err != nil {
			return ipc.Fail(ipc.CodeBadRequest, "force_resume takes no args")
		}
		return reply(h.d.ForceResume(ctx))

	case ipc.CmdSetEcoWatts:
		var args struct {
			Watts *int `json:"watts"`
		}
		if err := ipc.DecodeArgs(req.Args, &args); err != nil || args.Watts == nil {
			return ipc.Fail(ipc.CodeBadRequest, `set_eco_watts needs args {"watts": <integer>}`)
		}
		return reply(h.d.SetEcoWatts(ctx, *args.Watts))

	case ipc.CmdSetPauseProcesses:
		var args struct {
			Patterns *[]string `json:"patterns"`
		}
		if err := ipc.DecodeArgs(req.Args, &args); err != nil || args.Patterns == nil {
			return ipc.Fail(ipc.CodeBadRequest, `set_pause_processes needs args {"patterns": [<string>, ...]}`)
		}
		return reply(h.d.SetPauseProcesses(ctx, *args.Patterns))

	default:
		return ipc.Fail(ipc.CodeUnknownCmd, "unknown command")
	}
}

// reply turns a command result into a response: the new status on success,
// the mapped error code otherwise.
func reply(st daemon.Status, err error) ipc.Response {
	if err == nil {
		return ipc.OK(st)
	}
	return ipc.Fail(errorCode(err), err.Error())
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, daemon.ErrInvalidPatterns):
		return ipc.CodeBadRequest
	case errors.Is(err, daemon.ErrUnsupported):
		return ipc.CodeUnsupported
	case errors.Is(err, daemon.ErrOutOfRange):
		return ipc.CodeOutOfRange
	case errors.Is(err, daemon.ErrCapFailed):
		return ipc.CodeCapFailed
	case errors.Is(err, daemon.ErrGameActive):
		return ipc.CodeGameActive
	case errors.Is(err, daemon.ErrOnBattery):
		return ipc.CodeOnBattery
	case errors.Is(err, daemon.ErrThermal):
		return ipc.CodeThermal
	case errors.Is(err, daemon.ErrFAHUnavailable):
		return ipc.CodeFAHUnavailable
	case errors.Is(err, daemon.ErrBusy), errors.Is(err, context.DeadlineExceeded):
		return ipc.CodeBusy
	case errors.Is(err, daemon.ErrStopping), errors.Is(err, context.Canceled):
		return ipc.CodeStopping
	default:
		return ipc.CodeInternal
	}
}
