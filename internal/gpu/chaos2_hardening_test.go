package gpu

import (
	"math"
	"testing"
)

func TestChaos2MwToWCeilBoundaries(t *testing.T) {
	for mw, want := range map[uint32]int{0: 0, 1: 1, 999: 1, 1000: 1, 1001: 2, math.MaxUint32: 4294968} {
		if got := mwToWCeil(mw); got != want {
			t.Errorf("mwToWCeil(%d) = %d, want %d", mw, got, want)
		}
		if d := mwToWCeil(mw) - mwToW(mw); d < 0 || d > 1 {
			t.Errorf("ceil/floor disagree for %d", mw)
		}
	}
}
