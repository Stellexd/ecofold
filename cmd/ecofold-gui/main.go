// Command ecofold-gui is EcoFold's desktop client: a system tray icon and a
// control panel. It runs as the normal user, never as root, and does
// everything through the daemon's Unix socket.
package main

import (
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/Stellexd/ecofold/internal/gui"
	"github.com/Stellexd/ecofold/internal/ipc"
)

const (
	minPoll = 500 * time.Millisecond
	maxPoll = time.Minute
)

type options struct {
	socketPath string
	poll       time.Duration
	hidden     bool
}

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Without a default path the user must pass -socket-path; there is no
	// fallback to a shared directory.
	defSocket, sockErr := ipc.DefaultPath()
	var opts options
	flag.StringVar(&opts.socketPath, "socket-path", defSocket, "the daemon's Unix socket")
	flag.DurationVar(&opts.poll, "poll", gui.DefaultPoll, "status refresh interval (500ms-1m)")
	flag.BoolVar(&opts.hidden, "hidden", false, "start in the system tray without opening the control panel")
	flag.Parse()

	if opts.socketPath == "" {
		logger.Error("no socket path; pass -socket-path", "err", sockErr)
		return 2
	}
	if opts.poll < minPoll || opts.poll > maxPoll {
		logger.Error("invalid -poll", "poll", opts.poll, "min", minPoll, "max", maxPoll)
		return 2
	}
	if os.Geteuid() == 0 {
		logger.Warn("ecofold-gui is running as root; it is meant to run as the desktop user")
	}
	return runUI(logger, opts)
}
