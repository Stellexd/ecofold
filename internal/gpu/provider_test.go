package gpu

import (
	"math"
	"testing"
)

func TestValidatePowerLimit(t *testing.T) {
	min := 123
	max := 145

	cases := []struct {
		name    string
		watts   int
		wantErr bool
	}{
		{"below minimum (122)", 122, true},
		{"at minimum (123)", 123, false},
		{"mid range (130)", 130, false},
		{"at maximum (145)", 145, false},
		{"above maximum (146)", 146, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePowerLimit(tc.watts, min, max)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %dW, got nil", tc.watts)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %dW: %v", tc.watts, err)
			}
		})
	}
}

func TestMwToW(t *testing.T) {
	cases := []struct {
		mw   uint32
		want int
	}{
		{0, 0},
		{123000, 123},
		{145000, 145},
		{999, 0},
		{1999, 1},
	}

	for _, tc := range cases {
		got := mwToW(tc.mw)
		if got != tc.want {
			t.Errorf("mwToW(%d) = %d, want %d", tc.mw, got, tc.want)
		}
	}
}

func TestWToMW(t *testing.T) {
	maxValid := math.MaxUint32 / 1000

	cases := []struct {
		name    string
		w       int
		want    uint32
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"123W", 123, 123000, false},
		{"145W", 145, 145000, false},
		{"max valid", maxValid, uint32(maxValid * 1000), false},
		{"max+1", maxValid + 1, 0, true},
		{"negative", -5, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wToMW(tc.w)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("wToMW(%d) expected error, got nil", tc.w)
				}
				return
			}
			if err != nil {
				t.Fatalf("wToMW(%d) unexpected error: %v", tc.w, err)
			}
			if got != tc.want {
				t.Errorf("wToMW(%d) = %d, want %d", tc.w, got, tc.want)
			}
		})
	}
}

func TestValidatePowerLimitMinGreaterThanMax(t *testing.T) {
	// An inverted range must reject any value because it is impossible to satisfy
	// both bounds simultaneously.
	err := validatePowerLimit(130, 145, 123)
	if err == nil {
		t.Fatalf("expected error when min > max, got nil")
	}
}

func TestBytesToMiB(t *testing.T) {
	cases := []struct {
		bytes uint64
		want  uint64
	}{
		{0, 0},
		{1024 * 1024, 1},
		{1024 * 1024 * 512, 512},
		{1024 * 1024 * 1024, 1024},
		{1024*1024*1024 + 1, 1024},
	}

	for _, tc := range cases {
		got := bytesToMiB(tc.bytes)
		if got != tc.want {
			t.Errorf("bytesToMiB(%d) = %d, want %d", tc.bytes, got, tc.want)
		}
	}
}
