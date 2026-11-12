//go:build linux

package detector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// DefaultIgnoredProcesses lists process names that must never count as game
// load. Patterns are matched case-insensitively against both /proc/<pid>/comm
// and basename(argv[0]); a trailing "*" makes the pattern a prefix match.
//
// The Folding@home core is first on purpose: it keeps the GPU busy while
// folding, so counting it would pause folding forever.
var DefaultIgnoredProcesses = []string{
	"FahCore_*",
	"fahclient",
	"fahviewer",
	"fahcontrol",
	"foldingathome",
	"Xorg",
	"Xwayland",
	"gnome-shell",
	"mutter",
	"kwin_x11",
	"kwin_wayland",
	"sway",
	"Hyprland",
	"weston",
	"plasmashell",
	"steam",
	"steamwebhelper",
	"gamemoderun",
	"mangohud",
}

// browserBinaries are browsers whose GPU/renderer helpers hold VRAM and
// utilization without the user playing a game.
var browserBinaries = []string{
	"chrome",
	"chromium",
	"google-chrome",
	"google-chrome-stable",
	"firefox",
	"firefox-bin",
	"brave",
	"brave-browser",
	"msedge",
}

// LinuxCollector builds SystemSnapshot values on Linux from the GPU provider
// and per-process identity read from /proc.
//
// It never calls NVML directly: everything goes through gpu.Provider, so it
// compiles and runs with both the NVML provider (linux && cgo) and the Linux
// stub (linux && !cgo).
//
// LinuxCollector is not safe for concurrent use by multiple goroutines; each
// daemon tick creates one snapshot at a time. The provider it wraps is
// concurrency-safe.
type LinuxCollector struct {
	provider gpu.Provider
	ignored  []string
	// readFile is the /proc read seam. Production uses os.ReadFile.
	readFile func(string) ([]byte, error)
}

// NewLinuxCollector returns a collector backed by p with the default ignore
// list.
func NewLinuxCollector(p gpu.Provider) *LinuxCollector {
	return &LinuxCollector{
		provider: p,
		ignored:  append([]string(nil), DefaultIgnoredProcesses...),
		readFile: os.ReadFile,
	}
}

// Collect returns a snapshot with per-process GPU activity, filtered to the
// processes that are allowed to trigger a pause.
//
// Battery telemetry is out of scope on Linux for the MVP: the snapshot reports
// no battery, which keeps the evaluator in StateFolding unless real GPU load
// is detected. GPUDeviceUtilPct and GPUInUseMemMiB are deliberately left at
// zero because device-wide metrics are polluted by the Folding@home core; the
// evaluator uses GPUProcesses instead.
//
// A provider error is returned instead of an empty snapshot: the caller must
// keep its previous decision rather than feed "no load" to the evaluator.
func (c *LinuxCollector) Collect(ctx context.Context) (SystemSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return SystemSnapshot{}, err
	}
	if c.provider == nil {
		return SystemSnapshot{}, errors.New("detector: nil gpu provider")
	}

	tel, err := c.provider.GetTelemetry()
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("detector: gpu telemetry: %w", err)
	}
	procs, err := c.provider.GetGraphicsProcesses()
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("detector: gpu processes: %w", err)
	}

	if len(procs) > 0 {
		allInvalid := true
		for _, proc := range procs {
			if proc.SMUtilValid {
				allInvalid = false
				break
			}
		}
		if allInvalid {
			return SystemSnapshot{}, errors.New("detector: per-process GPU utilization not supported")
		}
	}

	kept := make([]gpu.ProcessInfo, 0, len(procs))
	for _, proc := range procs {
		if err := ctx.Err(); err != nil {
			return SystemSnapshot{}, err
		}
		ignored, gone := c.isIgnored(proc.PID)
		if ignored || gone {
			continue
		}
		kept = append(kept, proc)
	}

	return SystemSnapshot{
		HasBattery:   false,
		OnAC:         true,
		GPUClass:     tel.Name,
		GPUProcesses: kept,
	}, nil
}

// isIgnored resolves a PID to a process name and reports whether it must be
// filtered out. gone reports a process that disappeared between the NVML query
// and the /proc read: that race must drop the PID, not count it as load.
//
// Unreadable identity (permission denied, I/O error, empty name) is
// fail-closed: the process is kept and may count as load.
func (c *LinuxCollector) isIgnored(pid uint32) (ignored, gone bool) {
	read := c.readFile
	if read == nil {
		read = os.ReadFile
	}

	comm, err := read(procCommPath(pid))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, true
		}
		return false, false
	}
	if name := strings.TrimSpace(string(comm)); name != "" && c.matchesIgnored(name) {
		return true, false
	}

	raw, err := read(procCmdlinePath(pid))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, true
		}
		return false, false
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(argv) == 0 || argv[0] == "" {
		return false, false
	}
	base := filepath.Base(argv[0])
	if c.matchesIgnored(base) {
		return true, false
	}
	if isBrowserHelper(base, argv) {
		return true, false
	}
	return false, false
}

// matchesIgnored applies the case-insensitive exact/prefix rules.
func (c *LinuxCollector) matchesIgnored(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, pattern := range c.ignored {
		p := strings.ToLower(pattern)
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if name == p {
			return true
		}
	}
	return false
}

// isBrowserHelper reports whether base is a browser and argv identifies one of
// its GPU or renderer helper processes. Those helpers are not games.
func isBrowserHelper(base string, argv []string) bool {
	base = strings.ToLower(base)
	known := false
	for _, b := range browserBinaries {
		if base == b {
			known = true
			break
		}
	}
	if !known {
		return false
	}
	for _, arg := range argv[1:] {
		switch {
		case arg == "--type=gpu-process", arg == "--type=renderer":
			return true
		case strings.HasPrefix(arg, "--type=gpu-process="),
			strings.HasPrefix(arg, "--type=renderer="):
			return true
		case arg == "-contentproc": // Firefox content/GPU helper
			return true
		}
	}
	return false
}

func procCommPath(pid uint32) string {
	return fmt.Sprintf("/proc/%d/comm", pid)
}

func procCmdlinePath(pid uint32) string {
	return fmt.Sprintf("/proc/%d/cmdline", pid)
}
