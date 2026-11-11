//go:build chaos

package gpu

import "testing"

// CH2-3: rounding the minimum up while the maximum is floored inverts a tight
// NVML range: [123500, 123900] mW is advertised as Min 124 W > Max 123 W.
func TestChaos2CeilMinExceedsFlooredMax(t *testing.T) {
	minMW, maxMW := uint32(123500), uint32(123900)
	if lo, hi := mwToWCeil(minMW), mwToW(maxMW); lo > hi {
		t.Fatalf("advertised range inverted: MinPowerLimitW=%d > MaxPowerLimitW=%d for [%d,%d] mW", lo, hi, minMW, maxMW)
	}
}
