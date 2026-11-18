//go:build darwin

package detector

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxGPUClients bounds the user clients read per tick. A Mac has under a
// hundred; the limit only guards against a corrupt listing.
const maxGPUClients = 4096

// maxGPUApps caps the apps reported per snapshot.
const maxGPUApps = 16

// darwinIgnoredGPUApps are system processes whose GPU time is not an app the
// user runs: the compositor and the shell UI, Folding@home and EcoFold
// itself. Patterns follow DefaultIgnoredProcesses.
var darwinIgnoredGPUApps = []string{
	"WindowServer",
	"WindowManager",
	"Dock",
	"ControlCenter",
	"NotificationCent*",
	"SystemUIServer",
	"loginwindow",
	"FahCore_*",
	"fah-client",
	"ecofold-*",
}

// gpuClient is one IOAccelerator user client and the GPU time of its command
// queues so far, in nanoseconds.
type gpuClient struct {
	ID      uint64
	PID     uint32
	Name    string
	GPUTime uint64
}

// gpuSample is the last GPU time read for a user client.
type gpuSample struct {
	pid     uint32
	gpuTime uint64
}

// ioregClientRoot decodes only the user clients of the IOAccelerator nodes,
// loosely, so an unexpected type in one client drops that client instead of
// the whole reading.
type ioregClientRoot []struct {
	Children []map[string]any `plist:"IORegistryEntryChildren"`
}

// parseGPUClients returns the user clients in the output of
// FetchIOAcceleratorClientsContext that report AppUsage. ok is false when no
// client has an AppUsage array: this macOS does not publish it, or its schema
// changed, and per-app usage is unavailable. A client with a malformed
// creator or AppUsage entry is skipped.
func parseGPUClients(raw []byte) (clients []gpuClient, ok bool, err error) {
	var root ioregClientRoot
	if err := decodeXMLPlist(raw, &root); err != nil {
		return nil, false, fmt.Errorf("decode IOAccelerator clients: %w", err)
	}
	for _, node := range root {
		for _, child := range node.Children {
			usage, isArray := child["AppUsage"].([]any)
			if !isArray {
				continue
			}
			ok = true
			if len(clients) >= maxGPUClients {
				return clients, ok, nil
			}
			creator, _ := child["IOUserClientCreator"].(string)
			pid, name, good := parseClientCreator(creator)
			if !good {
				continue
			}
			total, good := sumGPUTime(usage)
			if !good {
				continue
			}
			id, isID := plistUint(child["IORegistryEntryID"])
			if !isID {
				continue
			}
			clients = append(clients, gpuClient{ID: id, PID: pid, Name: name, GPUTime: total})
		}
	}
	return clients, ok, nil
}

// parseClientCreator splits an IOUserClientCreator such as
// "pid 604, WindowServer". The name is p_comm, truncated to maxCommLen.
func parseClientCreator(s string) (pid uint32, name string, ok bool) {
	rest, found := strings.CutPrefix(s, "pid ")
	if !found {
		return 0, "", false
	}
	num, name, found := strings.Cut(rest, ", ")
	if !found {
		return 0, "", false
	}
	n, err := strconv.ParseUint(num, 10, 32)
	if err != nil || n == 0 {
		return 0, "", false
	}
	name = cleanProcName(name)
	if name == "" || len(name) > maxExecNameLen {
		return 0, "", false
	}
	return uint32(n), name, true
}

// sumGPUTime adds the accumulatedGPUTime of every AppUsage entry. An empty
// AppUsage is a client that never submitted work (0). An entry without a
// readable accumulatedGPUTime makes the client unreadable.
func sumGPUTime(usage []any) (uint64, bool) {
	var total uint64
	for _, e := range usage {
		entry, ok := e.(map[string]any)
		if !ok {
			return 0, false
		}
		v, ok := plistUint(entry["accumulatedGPUTime"])
		if !ok || total+v < total {
			return 0, false
		}
		total += v
	}
	return total, true
}

// plistUint returns a plist integer as uint64. howett.net/plist decodes an
// integer into an interface as uint64, or as int64 when it is negative.
func plistUint(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	}
	return 0, false
}

// gpuApps turns the clients of raw into per-app GPU time since the previous
// call. It never fails: a reading that cannot be parsed, or that has no
// AppUsage, reports nil and starts over, so the next good reading is a
// baseline again.
//
// Deltas are taken per user client, keyed by its registry ID, which is never
// reused while the system runs; a client whose PID changed, or whose time
// went backwards, is treated as new. A reused PID therefore never inherits
// the GPU time of the process it replaced.
func (c *DarwinCollector) gpuApps(raw []byte, at time.Time) []GPUAppUsage {
	clients, ok, err := parseGPUClients(raw)
	if err != nil || !ok {
		c.gpuPrev, c.gpuPrevAt = nil, time.Time{}
		return nil
	}
	prev, prevAt := c.gpuPrev, c.gpuPrevAt
	c.gpuPrev = make(map[uint64]gpuSample, len(clients))
	c.gpuPrevAt = at

	elapsed := at.Sub(prevAt)
	if prev == nil || prevAt.IsZero() || elapsed <= 0 {
		for _, cl := range clients {
			c.gpuPrev[cl.ID] = gpuSample{pid: cl.PID, gpuTime: cl.GPUTime}
		}
		return nil
	}

	self := uint32(os.Getpid())
	busy := map[uint32]uint64{}
	names := map[uint32]string{}
	for _, cl := range clients {
		c.gpuPrev[cl.ID] = gpuSample{pid: cl.PID, gpuTime: cl.GPUTime}
		p, seen := prev[cl.ID]
		if !seen || p.pid != cl.PID || cl.GPUTime < p.gpuTime || cl.PID == self {
			continue
		}
		if d := cl.GPUTime - p.gpuTime; d > 0 {
			busy[cl.PID] += d
			names[cl.PID] = cl.Name
		}
	}

	var apps []GPUAppUsage
	for pid, ns := range busy {
		name := names[pid]
		if len(name) >= maxCommLen {
			if full, err := c.execNameOf(int32(pid)); err == nil {
				name = full
			}
		}
		if matchPattern(darwinIgnoredGPUApps, name) {
			continue
		}
		pct := min(float64(ns)/float64(elapsed.Nanoseconds())*100, 100)
		apps = append(apps, GPUAppUsage{PID: pid, Name: name, GPUTimePct: int(pct + 0.5)})
	}
	slices.SortFunc(apps, func(a, b GPUAppUsage) int {
		return cmp.Or(cmp.Compare(b.GPUTimePct, a.GPUTimePct), cmp.Compare(a.Name, b.Name), cmp.Compare(a.PID, b.PID))
	})
	if len(apps) > maxGPUApps {
		apps = apps[:maxGPUApps]
	}
	return apps
}
