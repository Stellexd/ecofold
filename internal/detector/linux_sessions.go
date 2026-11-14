//go:build linux

package detector

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// Detection layer 2: game sessions.
//
// A GPU process belongs to a game session when one of its ancestors is a Steam
// launch (`reaper SteamLaunch AppId=N`, used for both Proton and native Steam
// games) or, outside Steam, when the process itself is a Wine/Proton .exe
// (Lutris, Heroic, plain wine). The session is anchored to that process: its
// PID plus its start time, so a reused PID never keeps a session alive.
//
// Sessions never trigger a pause on their own. The evaluator uses them to hold
// a load pause while the game that caused it is still running, so menus and
// loading screens do not start the cooldown. Native apps outside Steam (for
// example Blender) have no session and resume after the normal cooldown.

// maxAncestorDepth bounds the parent walk. Real launch chains are under ten
// levels deep; the bound also stops a malformed ppid loop.
const maxAncestorDepth = 32

// maxTrackedSessions bounds the sessions remembered between ticks. Dead ones
// are pruned every tick, so the bound only matters with dozens of games open.
const maxTrackedSessions = 32

// wineLoaders are Wine binaries whose first argument is the .exe they run.
var wineLoaders = []string{"wine", "wine64", "wine-preloader", "wine64-preloader"}

var errMalformedStat = errors.New("detector: malformed /proc stat")

// procStat holds the /proc/<pid>/stat fields layer 2 needs.
type procStat struct {
	ppid     uint32
	start    uint64 // starttime, field 22, in clock ticks since boot
	cpuTicks uint64 // utime + stime, fields 14 and 15, in clock ticks
	zombie   bool
}

// sessionAnchor identifies the process a session is tied to.
type sessionAnchor struct {
	pid   uint32
	start uint64
}

// parseProcStat parses /proc/<pid>/stat. The comm field can contain spaces and
// parentheses, so parsing starts after the last ')'.
func parseProcStat(raw []byte) (procStat, error) {
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, errMalformedStat
	}
	// After comm: state (field 3), ppid (4), ..., utime (14), stime (15),
	// ..., starttime (22).
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return procStat{}, errMalformedStat
	}
	ppid, err := strconv.ParseUint(f[1], 10, 32)
	if err != nil {
		return procStat{}, errMalformedStat
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return procStat{}, errMalformedStat
	}
	utime, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return procStat{}, errMalformedStat
	}
	stime, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return procStat{}, errMalformedStat
	}
	return procStat{
		ppid:     uint32(ppid),
		start:    start,
		cpuTicks: utime + stime,
		zombie:   f[0] == "Z" || f[0] == "X",
	}, nil
}

// gameSessions resolves the session of each GPU process and reports which
// tracked sessions are still alive. procs maps a kept GPU PID to its argv.
func (c *LinuxCollector) gameSessions(procs map[uint32][]string) (map[uint32]string, []string) {
	var bySession map[uint32]string
	for pid, argv := range procs {
		key, anchor, ok := c.sessionOf(pid, argv)
		if !ok {
			continue
		}
		if bySession == nil {
			bySession = make(map[uint32]string)
		}
		bySession[pid] = key
		if _, tracked := c.sessions[key]; !tracked && len(c.sessions) < maxTrackedSessions {
			if c.sessions == nil {
				c.sessions = make(map[string]sessionAnchor)
			}
			c.sessions[key] = anchor
		}
	}

	var live []string
	for key, a := range c.sessions {
		if c.anchorAlive(a) {
			live = append(live, key)
			continue
		}
		delete(c.sessions, key)
	}
	sort.Strings(live)
	return bySession, live
}

// sessionOf walks the ancestors of pid looking for a Steam launch, then falls
// back to pid itself being a Wine .exe.
func (c *LinuxCollector) sessionOf(pid uint32, argv []string) (string, sessionAnchor, bool) {
	self, err := c.stat(pid)
	if err != nil {
		return "", sessionAnchor{}, false
	}

	cur, st := pid, self
	for depth := 0; depth < maxAncestorDepth; depth++ {
		args := argv
		if cur != pid {
			args = c.argv(cur)
		}
		if appID, ok := steamLaunch(args); ok {
			return fmt.Sprintf("steam:%s/%d/%d", appID, cur, st.start), sessionAnchor{pid: cur, start: st.start}, true
		}
		if st.ppid <= 1 || st.ppid == cur {
			break
		}
		cur = st.ppid
		if st, err = c.stat(cur); err != nil {
			break
		}
	}

	if exe, ok := wineExe(argv); ok {
		return fmt.Sprintf("wine:%s/%d/%d", exe, pid, self.start), sessionAnchor{pid: pid, start: self.start}, true
	}
	return "", sessionAnchor{}, false
}

// anchorAlive reports whether the anchor process still runs. Only a missing
// process, a reused PID or a zombie ends a session; any other read error keeps
// it alive, which keeps the pause (fail-closed).
func (c *LinuxCollector) anchorAlive(a sessionAnchor) bool {
	st, err := c.stat(a.pid)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errMalformedStat)
	}
	return st.start == a.start && !st.zombie
}

func (c *LinuxCollector) stat(pid uint32) (procStat, error) {
	raw, err := c.read(procStatPath(pid))
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(raw)
}

// argv returns the command line of pid, or nil if it cannot be read.
func (c *LinuxCollector) argv(pid uint32) []string {
	raw, err := c.read(procCmdlinePath(pid))
	if err != nil {
		return nil
	}
	return splitCmdline(raw)
}

func splitCmdline(raw []byte) []string {
	s := strings.TrimSuffix(string(raw), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// steamLaunch reports whether argv is Steam's `reaper SteamLaunch` wrapper and
// returns its AppId ("unknown" when the argument is missing).
func steamLaunch(argv []string) (string, bool) {
	if len(argv) < 2 || anyBase(argv[0]) != "reaper" {
		return "", false
	}
	launch, appID := false, "unknown"
	for _, arg := range argv[1:] {
		if arg == "--" {
			break
		}
		switch {
		case arg == "SteamLaunch":
			launch = true
		case strings.HasPrefix(arg, "AppId="):
			if id := strings.TrimPrefix(arg, "AppId="); id != "" {
				appID = id
			}
		}
	}
	return appID, launch
}

// wineExe reports whether argv runs a Windows executable under Wine/Proton:
// either argv[0] is the .exe (how Wine renames its processes) or a Wine loader
// runs it as its first argument.
func wineExe(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	base := anyBase(argv[0])
	if strings.HasSuffix(base, ".exe") {
		return base, true
	}
	for _, loader := range wineLoaders {
		if base == loader && len(argv) > 1 {
			if exe := anyBase(argv[1]); strings.HasSuffix(exe, ".exe") {
				return exe, true
			}
		}
	}
	return "", false
}

// anyBase returns the lowercase last element of a Unix or Windows path. Wine
// processes report paths such as `C:\Games\game.exe`.
func anyBase(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	return strings.ToLower(path)
}

func procStatPath(pid uint32) string {
	return fmt.Sprintf("/proc/%d/stat", pid)
}
