//go:build darwin && chaos

// Chaos repro tests (fail on the commit under test). See docs/CHAOS_REPORT.md.
// Run with: go test -tags chaos -race -run Chaos -v ./internal/detector
package detector

import "testing"

// CH-6: PerformanceStatistics is decoded as map[string]uint64, so a single
// entry of another type, in a key EcoFold never reads, fails the whole
// IOAccelerator decode. GPU telemetry is then lost entirely (the game-load
// layer is blind) whenever a macOS update or another Apple GPU adds, for
// example, a string or real statistic next to the integer ones.
func TestChaosForeignPerformanceStatisticTypeBreaksGPUParse(t *testing.T) {
	for _, extra := range []string{
		`<key>Driver Version</key><string>1.2.3</string>`,
		`<key>Average Frequency</key><real>1398.5</real>`,
		`<key>Per-Core Utilization</key><array><integer>1</integer></array>`,
		`<key>Throttled</key><false/>`,
	} {
		raw := []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array><dict>` +
			`<key>IOClass</key><string>AGXAcceleratorG16G</string><key>PerformanceStatistics</key><dict>` +
			`<key>Device Utilization %</key><integer>97</integer>` +
			`<key>In use system memory</key><integer>4294967296</integer>` + extra +
			`</dict></dict></array></plist>`)
		m, err := ParseIOAcceleratorPlist(raw)
		if err != nil || m.GPUDeviceUtilPct != 97 || m.GPUInUseMemMiB != 4096 {
			t.Errorf("with unrelated stat %s: got %+v, err=%v; want util 97, 4096 MiB", extra, m, err)
		}
	}
}
