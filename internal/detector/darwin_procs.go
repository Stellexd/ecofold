//go:build darwin

package detector

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// maxCommLen is MAXCOMLEN, the length p_comm is truncated to. A name of
// exactly this length may be the start of a longer executable name.
const maxCommLen = 16

// maxProcEntries bounds the processes examined per scan. A Mac runs a few
// hundred to a couple of thousand; the limit only guards against a corrupt
// listing.
const maxProcEntries = 1 << 16

// maxExecNameLen bounds the executable name read from kern.procargs2.
// Executable names are at most NAME_MAX (255) bytes.
const maxExecNameLen = 255

// procEntry is one process of the kern.proc.all listing.
type procEntry struct {
	PID  int32
	Comm string
}

// listDarwinProcs lists every process with sysctl kern.proc.all. It needs no
// privileges and runs no external command.
func listDarwinProcs() ([]procEntry, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]procEntry, 0, min(len(kps), maxProcEntries))
	for i := range kps {
		if len(out) >= maxProcEntries {
			break
		}
		p := &kps[i].Proc
		if p.P_pid <= 0 {
			continue
		}
		out = append(out, procEntry{PID: p.P_pid, Comm: commString(p.P_comm[:])})
	}
	return out, nil
}

// commString decodes a NUL-padded p_comm. It never returns more than
// maxCommLen bytes.
func commString(raw []byte) string {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) > maxCommLen {
		raw = raw[:maxCommLen]
	}
	return cleanProcName(string(raw))
}

// darwinExecName returns the executable's file name from kern.procargs2.
// It fails for processes of other users unless the daemon runs as root, and
// for processes that already exited.
func darwinExecName(pid int32) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return "", err
	}
	return parseProcArgs(raw)
}

// errBadProcArgs is returned for a kern.procargs2 buffer that cannot be
// parsed.
var errBadProcArgs = errors.New("detector: malformed kern.procargs2")

// parseProcArgs returns the base name of the executable path in a
// kern.procargs2 buffer: a 32-bit argc, then the NUL-terminated path.
func parseProcArgs(raw []byte) (string, error) {
	if len(raw) < 4 || int32(binary.LittleEndian.Uint32(raw)) < 0 {
		return "", errBadProcArgs
	}
	path := raw[4:]
	end := bytes.IndexByte(path, 0)
	if end <= 0 {
		return "", errBadProcArgs
	}
	path = path[:end]
	name := path[bytes.LastIndexByte(path, '/')+1:]
	if len(name) > maxExecNameLen {
		return "", errBadProcArgs
	}
	if s := cleanProcName(string(name)); s != "" {
		return s, nil
	}
	return "", errBadProcArgs
}

// cleanProcName trims a process name and rejects one that is not valid
// UTF-8 or holds control characters: such a name never matches a pattern
// and must not reach the status.
func cleanProcName(s string) string {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return ""
	}
	return s
}

// SetPauseProcesses sets the user's pause list. Patterns must come from
// ParsePauseProcesses. While a running process matches, every snapshot reports
// it in UserPauseMatches.
func (c *DarwinCollector) SetPauseProcesses(patterns []string) {
	c.pause = append([]string(nil), patterns...)
}

// userPauseMatches lists the processes once and returns the sorted,
// deduplicated names that match the pause list. It does nothing while the
// list is empty. A listing error yields no matches, as on Linux: the pause
// list is a preference and must not fail Collect.
//
// p_comm is truncated to maxCommLen bytes, so a name of that length that a
// pattern could extend is resolved to the executable name before matching.
// A process that exited in between keeps its p_comm.
func (c *DarwinCollector) userPauseMatches(ctx context.Context) []string {
	if len(c.pause) == 0 {
		return nil
	}
	list := c.listProcs
	if list == nil {
		list = listDarwinProcs
	}
	procs, err := list()
	if err != nil {
		return nil
	}
	found := map[string]bool{}
	for i, p := range procs {
		if i >= maxProcEntries || ctx.Err() != nil || len(found) >= maxPauseMatches {
			break
		}
		comm := cleanProcName(p.Comm)
		if addPauseMatch(found, c.pause, comm) || !c.mayBeTruncated(comm) {
			continue
		}
		if name, err := c.execNameOf(p.PID); err == nil {
			addPauseMatch(found, c.pause, name)
		}
	}
	return sortedMatches(found)
}

// mayBeTruncated reports whether comm has p_comm's full length and a pattern
// is longer and starts with it, so the executable name could match where
// comm did not.
func (c *DarwinCollector) mayBeTruncated(comm string) bool {
	if len(comm) < maxCommLen {
		return false
	}
	comm = strings.ToLower(comm)
	for _, p := range c.pause {
		stem := strings.TrimSuffix(p, "*")
		if len(stem) > len(comm) && strings.HasPrefix(stem, comm) {
			return true
		}
	}
	return false
}

// execNameOf is the kern.procargs2 seam with the darwinExecName default.
func (c *DarwinCollector) execNameOf(pid int32) (string, error) {
	if c.execName == nil {
		return darwinExecName(pid)
	}
	return c.execName(pid)
}
