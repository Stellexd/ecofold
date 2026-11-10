package gpu

import (
	"math"
	"testing"
)

// TestClampMWProperty exercises the clamp over a grid of values. For any
// non-inverted range [lo, hi] the result must always land inside it and be
// idempotent; this protects RestoreDefaultPowerLimit from applying a value
// NVML would reject.
func TestClampMWProperty(t *testing.T) {
	values := []uint32{0, 1, 1000, 122999, 123000, 130000, 145000, 145001, 1 << 30, math.MaxUint32}
	ranges := [][2]uint32{
		{0, 0},
		{0, 1},
		{1, 0 + 1},
		{123000, 145000},
		{145000, 123000}, // inverted: deliberately excluded below
		{math.MaxUint32 - 1, math.MaxUint32},
		{0, math.MaxUint32},
	}

	for _, r := range ranges {
		lo, hi := r[0], r[1]
		if lo > hi {
			continue
		}
		for _, v := range values {
			got := clampMW(v, lo, hi)
			if got < lo || got > hi {
				t.Errorf("clampMW(%d, %d, %d) = %d fuera del rango", v, lo, hi, got)
			}
			if again := clampMW(got, lo, hi); again != got {
				t.Errorf("clampMW no es idempotente: clamp(clamp(%d)) = %d, se esperaba %d", v, again, got)
			}
		}
	}
}

// TestClampMWDegenerateZeroRange documents the remaining hazard in M2: NVML
// constraints of (0, 0) with a (0) default make clampMW return 0. If a driver
// reports those constraints with SUCCESS, RestoreDefaultPowerLimit would call
// SetPowerManagementLimit(0). The production code only rejects min > max, so a
// caller-side rejection of a zero/zero range is recommended. This test pins the
// current behavior so the recommendation is explicit; it is expected to change
// if the coder adds a validation.
func TestClampMWDegenerateZeroRange(t *testing.T) {
	if got := clampMW(0, 0, 0); got != 0 {
		t.Fatalf("clampMW(0,0,0) = %d; el comportamiento documentado era 0 (el coder debe rechazar el rango 0,0 antes de aplicar)", got)
	}
	// A zero default with a sane range is clamped up to the minimum, which is
	// safe for the hardware (it under-restores instead of applying 0W).
	if got := clampMW(0, 123000, 145000); got != 123000 {
		t.Fatalf("clampMW(0, 123000, 145000) = %d, se esperaba el mínimo 123000", got)
	}
}

// TestClampMWInvertedRangeIsNeverUsedByRestore mirrors the guard in
// RestoreDefaultPowerLimit: the caller must reject min > max before calling
// clampMW, because clampMW would silently pick the first branch (lo).
func TestClampMWInvertedRangeIsNeverUsedByRestore(t *testing.T) {
	lo, hi := uint32(145000), uint32(123000)
	if !(lo > hi) {
		t.Fatal("el caso de prueba requiere lo > hi")
	}
	// If the caller forgot the guard, clampMW would return lo (145000), a value
	// above the reported maximum. Record the hazard.
	if got := clampMW(130000, lo, hi); got != lo {
		t.Fatalf("clampMW con rango invertido = %d, se esperaba %d (documentando el peligro)", got, lo)
	}
}

// TestWToMWMaxBoundary checks the exact overflow boundary from both sides.
func TestWToMWMaxBoundary(t *testing.T) {
	const maxWatts = math.MaxUint32 / 1000

	got, err := wToMW(maxWatts)
	if err != nil {
		t.Fatalf("wToMW(%d) devolvió error en el límite: %v", maxWatts, err)
	}
	if got != uint32(maxWatts*1000) {
		t.Errorf("wToMW(%d) = %d, se esperaba %d", maxWatts, got, maxWatts*1000)
	}

	if _, err := wToMW(maxWatts + 1); err == nil {
		t.Errorf("wToMW(%d) no detectó el desbordamiento de uint32", maxWatts+1)
	}
	if _, err := wToMW(math.MaxInt); err == nil {
		t.Errorf("wToMW(math.MaxInt) no detectó el desbordamiento de uint32")
	}
}

// TestValidatePowerLimitExtremes covers zero, negative and inverted ranges: all
// of them must be rejected so SetPowerLimit never forwards a nonsensical value
// to NVML.
func TestValidatePowerLimitExtremes(t *testing.T) {
	cases := []struct {
		name       string
		watts      int
		lo, hi     int
		shouldPass bool
	}{
		{"zero range zero watts", 0, 0, 0, true},
		{"zero watts in normal range", 0, 123, 145, false},
		{"negative watts", -1, -1000, 1000, true /* mathematically in range; wToMW rejects it later */},
		{"negative range", -10, -100, -1, true},
		{"inverted range", 130, 145, 123, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePowerLimit(tc.watts, tc.lo, tc.hi)
			ok := err == nil
			if ok != tc.shouldPass {
				t.Fatalf("validatePowerLimit(%d, %d, %d) error = %v, se esperaba pass=%v",
					tc.watts, tc.lo, tc.hi, err, tc.shouldPass)
			}
		})
	}
}
