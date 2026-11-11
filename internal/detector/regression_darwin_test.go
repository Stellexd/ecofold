//go:build darwin

// Regression tests for the m1 chaos pass findings (docs/CHAOS_REPORT.md).
package detector

import "testing"

// CH-6: an entry of an unexpected type in a PerformanceStatistics key EcoFold
// never reads must not discard the whole GPU telemetry.
func TestForeignPerformanceStatisticTypeKeepsGPUParse(t *testing.T) {
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

// Read keys with a non-integer type degrade to 0 (or are truncated if they are
// a non-negative real) instead of failing the parse.
func TestPerformanceStatisticTypesDegrade(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array><dict>` +
		`<key>IOClass</key><string>AGX</string><key>PerformanceStatistics</key><dict>` +
		`<key>Device Utilization %</key><real>55.9</real>` +
		`<key>Renderer Utilization %</key><string>high</string>` +
		`<key>Tiler Utilization %</key><integer>-4</integer>` +
		`<key>In use system memory</key><real>1e300</real>` +
		`</dict></dict></array></plist>`)
	m, err := ParseIOAcceleratorPlist(raw)
	if err != nil {
		t.Fatalf("ParseIOAcceleratorPlist: %v", err)
	}
	if m.GPUDeviceUtilPct != 55 || m.GPURendererUtilPct != 0 || m.GPUTilerUtilPct != 0 || m.GPUInUseMemMiB != 0 {
		t.Fatalf("got %+v; want util 55, renderer 0, tiler 0, mem 0", m)
	}
}
