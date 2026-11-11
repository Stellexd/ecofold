//go:build darwin && chaos

package detector

import "testing"

// CH2-2: a PerformanceStatistics <integer> above MaxInt64 decodes as uint64,
// statUint passes it through, and int(...) wraps it to a negative percentage.
func TestChaos2HugeIntegerStatWrapsNegative(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array><dict>` +
		`<key>IOClass</key><string>AGXAcceleratorG16G</string><key>PerformanceStatistics</key><dict>` +
		`<key>Device Utilization %</key><integer>18446744073709551615</integer>` +
		`<key>Renderer Utilization %</key><integer>9223372036854775808</integer>` +
		`</dict></dict></array></plist>`)
	m, err := ParseIOAcceleratorPlist(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.GPUDeviceUtilPct < 0 || m.GPURendererUtilPct < 0 {
		t.Fatalf("utilization wrapped negative: device=%d renderer=%d", m.GPUDeviceUtilPct, m.GPURendererUtilPct)
	}
}
