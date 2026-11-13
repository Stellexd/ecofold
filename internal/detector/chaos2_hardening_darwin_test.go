//go:build darwin

package detector

import (
	"math"
	"testing"
)

// statUint must map every non-finite, negative or foreign input to 0 and
// never panic.
func TestChaos2StatUintEdgeValues(t *testing.T) {
	cases := []struct {
		v    any
		want uint64
	}{
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0}, {math.Copysign(0, -1), 0},
		{float64(math.MaxInt64), 0}, {math.Nextafter(float64(math.MaxInt64), 0), 9223372036854774784},
		{int64(math.MinInt64), 0}, {int64(-1), 0}, {nil, 0}, {map[string]any{}, 0}, {[]any{1}, 0},
		{true, 0}, {"5", 0}, {uint64(7), 7}, {0.99, 0},
	}
	for _, c := range cases {
		if got := statUint(map[string]any{"k": c.v}, "k"); got != c.want {
			t.Errorf("statUint(%#v) = %d, want %d", c.v, got, c.want)
		}
	}
}
