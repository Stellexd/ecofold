//go:build linux && cgo && nvml_integration

package gpu

import (
	"testing"
)

// TestNvidiaProcessIntegration exercises the read-only per-process graphics
// process query against the real NVML driver. It does not change the power
// limit, so it can run without root.
func TestNvidiaProcessIntegration(t *testing.T) {
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
				t.Errorf("call %d: PID %d SMUtilPct=%d out of [0,100]", call, pr.PID, pr.SMUtilPct)
			}
		}
	}
}
