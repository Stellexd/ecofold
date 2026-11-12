package gpu

// Regression tests for the m1 chaos pass findings (docs/CHAOS_REPORT.md).

import (
	"math"
	"testing"
)

// CH-7: Telemetry.MinPowerLimitW is computed with mwToWCeil, so the advertised
// minimum always passes the milliwatt range check that SetPowerLimit applies
// (nvidia_linux.go), even when the NVML minimum is not a whole number of watts.
func TestAdvertisedMinPowerLimitIsSettable(t *testing.T) {
	for _, c := range []struct{ minMW, maxMW uint32 }{
		{123000, 145000}, // RTX 5060: exact watts
		{123500, 145000}, // fractional minimum
		{100001, 200000},
		{70999, 115000},
	} {
		advertised := mwToWCeil(c.minMW)
		mw, err := wToMW(advertised)
		if err != nil {
			t.Fatalf("wToMW(%d): %v", advertised, err)
		}
		if mw < c.minMW || mw > c.maxMW {
			t.Errorf("constraints [%d, %d] mW: MinPowerLimitW=%dW is rejected (%d mW)", c.minMW, c.maxMW, advertised, mw)
		}
	}
}

func TestMwToWCeil(t *testing.T) {
	for _, c := range []struct {
		mw   uint32
		want int
	}{
		{0, 0}, {1, 1}, {999, 1}, {1000, 1}, {1001, 2}, {123000, 123}, {123500, 124},
		{math.MaxUint32, 4294968},
	} {
		if got := mwToWCeil(c.mw); got != c.want {
			t.Errorf("mwToWCeil(%d) = %d, want %d", c.mw, got, c.want)
		}
	}
}
