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

// TestNvidiaProcessUtilizationIsReadOnly exercises the real per-process
// query without root: it must return percentages in range and keep working
// across calls. It skips when no NVIDIA driver is available.
func TestNvidiaProcessUtilizationIsReadOnly(t *testing.T) {
	p := &NvidiaProvider{}
	if err := p.Init(); err != nil {
		t.Skipf("NVML unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown() })

	for call := 1; call <= 2; call++ {
		procs, err := p.GetGraphicsProcesses()
		if err != nil {
			t.Fatalf("call %d: GetGraphicsProcesses: %v", call, err)
		}
		for _, pr := range procs {
			if pr.SMUtilValid && (pr.SMUtilPct < 0 || pr.SMUtilPct > 100) {
				t.Errorf("call %d: PID %d SMUtilPct = %d fuera de [0,100]", call, pr.PID, pr.SMUtilPct)
			}
		}
	}
}
