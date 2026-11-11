//go:build darwin

// Regression tests for the second chaos pass (docs/CHAOS_REPORT_2.md).

package detector

import "testing"

// CH2-2: a PerformanceStatistics <integer> above MaxInt64 must not wrap to a
// negative percentage; absurd values saturate at 100 (reads as high load).
func TestHugeIntegerStatSaturates(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array><dict>` +
		`<key>IOClass</key><string>AGXAcceleratorG16G</string><key>PerformanceStatistics</key><dict>` +
		`<key>Device Utilization %</key><integer>18446744073709551615</integer>` +
		`<key>Renderer Utilization %</key><integer>9223372036854775808</integer>` +
		`</dict></dict></array></plist>`)
	m, err := ParseIOAcceleratorPlist(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.GPUDeviceUtilPct != 100 || m.GPURendererUtilPct != 100 {
		t.Fatalf("got device=%d renderer=%d, want both saturated at 100", m.GPUDeviceUtilPct, m.GPURendererUtilPct)
	}
}
