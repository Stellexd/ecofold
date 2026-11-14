//go:build linux && cgo

package gpu

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// TestMergeUtilizationSamplesKeepsLatestPerPID pins the per-PID sample
// selection: the newest timestamp wins and older out-of-order samples are
// discarded.
func TestMergeUtilizationSamplesKeepsLatestPerPID(t *testing.T) {
	samples := []nvml.ProcessUtilizationSample{
		{Pid: 1, TimeStamp: 10, SmUtil: 30},
		{Pid: 1, TimeStamp: 20, SmUtil: 70},
		{Pid: 1, TimeStamp: 15, SmUtil: 90}, // older: must not win
		{Pid: 2, TimeStamp: 20, SmUtil: 5},
		{Pid: 3, TimeStamp: 20, SmUtil: 250}, // clamp to 100
	}

	byPID, high := mergeUtilizationSamples(0, samples)
	if high != 20 {
		t.Errorf("high-water timestamp = %d, se esperaba 20", high)
	}
	if got := byPID[1]; got != 70 {
		t.Errorf("PID 1 = %d, se esperaba 70 (muestra más reciente)", got)
	}
	if got := byPID[2]; got != 5 {
		t.Errorf("PID 2 = %d, se esperaba 5", got)
	}
	if got := byPID[3]; got != 100 {
		t.Errorf("PID 3 = %d, se esperaba 100 (clamp)", got)
	}

	// Timestamps older than the last seen value never move it backwards.
	if _, high := mergeUtilizationSamples(50, samples); high != 50 {
		t.Errorf("high-water = %d, se esperaba 50", high)
	}
	if byPID, _ := mergeUtilizationSamples(0, nil); len(byPID) != 0 {
		t.Errorf("sin muestras: byPID = %v, se esperaba vacío", byPID)
	}
}

// TestClassifyUtilReturn pins the mapping of GetProcessUtilization return
// codes to the behaviour expected by processUtilization.
func TestClassifyUtilReturn(t *testing.T) {
	cases := []struct {
		ret            nvml.Return
		wantUtilizable bool
		wantAdvance    bool
		wantErr        bool
	}{
		{ret: nvml.SUCCESS, wantUtilizable: true, wantAdvance: true},
		{ret: nvml.ERROR_NOT_FOUND, wantUtilizable: true, wantAdvance: false},
		{ret: nvml.ERROR_INSUFFICIENT_SIZE, wantUtilizable: true, wantAdvance: true},
		{ret: nvml.ERROR_NOT_SUPPORTED, wantUtilizable: false, wantAdvance: false},
		{ret: nvml.ERROR_UNKNOWN, wantUtilizable: false, wantAdvance: false, wantErr: true},
	}
	for _, c := range cases {
		utilizable, advance, err := classifyUtilReturn(c.ret)
		if utilizable != c.wantUtilizable || advance != c.wantAdvance || (err != nil) != c.wantErr {
			t.Errorf("classifyUtilReturn(%v) = (utilizable=%v, advance=%v, err=%v), want (%v, %v, err=%v)",
				c.ret, utilizable, advance, err, c.wantUtilizable, c.wantAdvance, c.wantErr)
		}
	}
}

// TestClampPercent covers the percentage clamp used on NVML values.
func TestClampPercent(t *testing.T) {
	cases := []struct{ in, want int }{
		{in: -1, want: 0},
		{in: 0, want: 0},
		{in: 45, want: 45},
		{in: 100, want: 100},
		{in: 101, want: 100},
	}
	for _, c := range cases {
		if got := clampPercent(c.in); got != c.want {
			t.Errorf("clampPercent(%d) = %d, se esperaba %d", c.in, got, c.want)
		}
	}
}

// TestMergeProcessListsDeduplicatesCPlusG pins the graphics/compute join: a
// C+G process is kept once with the larger memory figure, and compute-only
// processes (CUDA renders, the FAH core) are added.
func TestMergeProcessListsDeduplicatesCPlusG(t *testing.T) {
	graphics := []nvml.ProcessInfo{
		{Pid: 10, UsedGpuMemory: 100}, // kwin, C+G
		{Pid: 20, UsedGpuMemory: 300}, // game
	}
	compute := []nvml.ProcessInfo{
		{Pid: 10, UsedGpuMemory: 150},
		{Pid: 30, UsedGpuMemory: 800}, // FahCore or Blender
		{Pid: 30, UsedGpuMemory: 900}, // second context of the same PID
	}
	got := mergeProcessLists(graphics, compute)
	want := []nvml.ProcessInfo{{Pid: 10, UsedGpuMemory: 150}, {Pid: 20, UsedGpuMemory: 300}, {Pid: 30, UsedGpuMemory: 900}}
	if len(got) != len(want) {
		t.Fatalf("merge = %+v, se esperaba %+v", got, want)
	}
	for i := range want {
		if got[i].Pid != want[i].Pid || got[i].UsedGpuMemory != want[i].UsedGpuMemory {
			t.Fatalf("merge[%d] = %+v, se esperaba %+v", i, got[i], want[i])
		}
	}
	if got := mergeProcessLists(nil, nil); len(got) != 0 {
		t.Fatalf("listas vacías: %+v", got)
	}
}
