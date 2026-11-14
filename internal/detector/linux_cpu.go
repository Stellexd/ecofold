//go:build linux

package detector

import "time"

// userHZ is the kernel's USER_HZ, the unit of the /proc/<pid>/stat CPU
// fields. It is 100 on every Linux architecture EcoFold targets.
const userHZ = 100

// cpuSample is the last CPU reading of a compute process.
type cpuSample struct {
	start uint64 // process start time, to detect a reused PID
	ticks uint64
	at    time.Time
}

// computeCPU returns the CPU use, in percent of one core, of each compute
// process in procs since the previous Collect. A process seen for the first
// time, or whose PID was reused, has no entry until the next tick. Entries
// for processes that left the GPU are dropped.
func (c *LinuxCollector) computeCPU(pids []uint32) map[uint32]int {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	at := now()
	var out map[uint32]int
	next := make(map[uint32]cpuSample, len(pids))
	for _, pid := range pids {
		st, err := c.stat(pid)
		if err != nil {
			continue
		}
		cur := cpuSample{start: st.start, ticks: st.cpuTicks, at: at}
		next[pid] = cur
		prev, ok := c.cpuPrev[pid]
		if !ok || prev.start != cur.start || cur.ticks < prev.ticks {
			continue
		}
		elapsed := cur.at.Sub(prev.at)
		if elapsed <= 0 {
			continue
		}
		pct := float64(cur.ticks-prev.ticks) / userHZ / elapsed.Seconds() * 100
		if out == nil {
			out = make(map[uint32]int)
		}
		out[pid] = int(pct + 0.5)
	}
	c.cpuPrev = next
	return out
}
