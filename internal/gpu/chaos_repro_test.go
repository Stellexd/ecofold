//go:build chaos

// Chaos repro tests (fail on the commit under test). See docs/CHAOS_REPORT.md.
// Run with: go test -tags chaos -race -run Chaos -v ./internal/gpu
package gpu

import "testing"

// CH-7: Telemetry reports power constraints with floor division (mwToW), but
// NvidiaProvider.SetPowerLimit converts back with wToMW and range-checks
// against the raw milliwatt constraints (nvidia_linux.go:117-127). When the
// NVML minimum is not a whole number of watts, the advertised
// MinPowerLimitW is below the real minimum, so SetPowerLimit(MinPowerLimitW)
// (the natural "eco" value for a daemon or GUI) is always rejected. The same
// happens to DefaultPowerLimitW / PowerLimitW when they sit on the minimum.
//
// This test replays the exact conversions and range check of
// nvidia_linux.go with constraint values that NVML can legally report.
func TestChaosAdvertisedMinPowerLimitIsNotSettable(t *testing.T) {
	for _, c := range []struct{ minMW, maxMW uint32 }{
		{123500, 145000}, // fractional minimum
		{100001, 200000},
		{70999, 115000},
	} {
		advertised := mwToW(c.minMW) // Telemetry.MinPowerLimitW
		mw, err := wToMW(advertised)
		if err != nil {
			t.Fatalf("wToMW(%d): %v", advertised, err)
		}
		if mw < c.minMW || mw > c.maxMW { // same check as NvidiaProvider.SetPowerLimit
			t.Errorf("constraints [%d, %d] mW: Telemetry.MinPowerLimitW=%dW, but SetPowerLimit(%d) "+
				"is rejected (%d mW < %d mW)", c.minMW, c.maxMW, advertised, advertised, mw, c.minMW)
		}
	}
}
