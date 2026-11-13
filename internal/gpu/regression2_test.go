package gpu

// Regression tests for the second chaos pass (docs/CHAOS_REPORT_2.md).

import "testing"

// CH2-3: rounding the minimum up and the maximum down must never advertise an
// inverted range; when no whole watt fits, the limit is reported as not
// settable.
func TestPowerRangeW(t *testing.T) {
	for _, c := range []struct {
		minMW, maxMW uint32
		lo, hi       int
		ok           bool
	}{
		{123000, 145000, 123, 145, true}, // RTX 5060
		{123500, 145000, 124, 145, true},
		{123000, 123000, 123, 123, true},
		{123500, 123900, 123, 123, false}, // no whole watt in range
		{0, 0, 0, 0, true},
	} {
		lo, hi, ok := powerRangeW(c.minMW, c.maxMW)
		if lo != c.lo || hi != c.hi || ok != c.ok {
			t.Errorf("powerRangeW(%d, %d) = %d, %d, %t; want %d, %d, %t",
				c.minMW, c.maxMW, lo, hi, ok, c.lo, c.hi, c.ok)
		}
		if lo > hi {
			t.Errorf("powerRangeW(%d, %d) advertises an inverted range %d > %d", c.minMW, c.maxMW, lo, hi)
		}
	}
}
