package gpu

import (
	"math"
	"testing"
)

// Hardening fuzzers from the m1 chaos pass (docs/CHAOS_REPORT.md).

func FuzzPowerConversions(f *testing.F) {
	f.Add(0, uint32(0), uint32(0), uint32(0), uint64(0))
	f.Add(145, uint32(123000), uint32(145000), uint32(123999), uint64(math.MaxUint64))
	f.Add(math.MaxUint32/1000, uint32(math.MaxUint32), uint32(1), uint32(math.MaxUint32), uint64(math.MaxUint64-1))
	f.Add(-1, uint32(5), uint32(5), uint32(4), uint64(1<<20))
	f.Fuzz(func(t *testing.T, w int, lo, hi, mw uint32, b uint64) {
		got, err := wToMW(w)
		switch {
		case w < 0 || w > math.MaxUint32/1000:
			if err == nil {
				t.Fatalf("wToMW(%d) accepted an out-of-range value: %d", w, got)
			}
		case err != nil:
			t.Fatalf("wToMW(%d): %v", w, err)
		case uint64(got) != uint64(w)*1000:
			t.Fatalf("wToMW(%d) = %d", w, got)
		case mwToW(got) != w:
			t.Fatalf("round trip %d W -> %d mW -> %d W", w, got, mwToW(got))
		}

		if w := mwToW(mw); w < 0 || uint64(w)*1000 > uint64(mw) || uint64(w+1)*1000 <= uint64(mw) {
			t.Fatalf("mwToW(%d) = %d is not floor(mw/1000)", mw, w)
		}

		if lo <= hi {
			if c := clampMW(mw, lo, hi); c < lo || c > hi || (mw >= lo && mw <= hi && c != mw) {
				t.Fatalf("clampMW(%d, %d, %d) = %d", mw, lo, hi, c)
			}
		}

		if m := bytesToMiB(b); b != math.MaxUint64 && m != b>>20 {
			t.Fatalf("bytesToMiB(%d) = %d", b, m)
		}
	})
}
