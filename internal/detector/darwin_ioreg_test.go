//go:build darwin

package detector

import (
	"os"
	"path/filepath"
	"testing"
)

// emptyBatteryPlist simulates ioreg output on a desktop Mac (Mac mini,
// Mac Studio) where AppleSmartBattery does not exist and the root is an empty array.
const emptyBatteryPlist = `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array/></plist>`

// readFixture loads a fixture from testdata and fails the test if it does not
// exist or cannot be read. It accepts testing.TB for reuse in both tests and fuzzing.
func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		tb.Fatalf("no se pudo leer el fixture %q: %v", name, err)
	}
	if len(raw) == 0 {
		tb.Fatalf("el fixture %q está vacío", name)
	}
	return raw
}

// TestParseChargingFixture validates electrical extraction from a plugged-in
// and charging MacBook Air M4: all fields must preserve their sign and units.
func TestParseChargingFixture(t *testing.T) {
	raw := readFixture(t, "m4_air_battery_charging.plist")

	got, err := ParseSmartBatteryPlist(raw)
	if err != nil {
		t.Fatalf("ParseSmartBatteryPlist(charging) devolvió error inesperado: %v", err)
	}

	if !got.HasBattery {
		t.Errorf("HasBattery = false, se esperaba true")
	}
	if !got.OnAC {
		t.Errorf("OnAC = false, se esperaba true")
	}
	if !got.IsCharging {
		t.Errorf("IsCharging = false, se esperaba true")
	}
	if got.AmperageMA != 898 {
		t.Errorf("AmperageMA = %d, se esperaba 898", got.AmperageMA)
	}
	if got.BatteryPowerMW != 10330 {
		t.Errorf("BatteryPowerMW = %d, se esperaba 10330", got.BatteryPowerMW)
	}
	if got.WallInputMW != 29800 {
		t.Errorf("WallInputMW = %d, se esperaba 29800", got.WallInputMW)
	}
	if got.TimeRemainingMin != nil {
		t.Errorf("TimeRemainingMin = %v, se esperaba nil (centinela 65535)", *got.TimeRemainingMin)
	}
}

// TestParseDischargingFixture validates negative two's-complement values and
// conversion of the remaining-time sentinel.
func TestParseDischargingFixture(t *testing.T) {
	raw := readFixture(t, "m4_air_battery_discharging.plist")

	got, err := ParseSmartBatteryPlist(raw)
	if err != nil {
		t.Fatalf("ParseSmartBatteryPlist(discharging) devolvió error inesperado: %v", err)
	}

	if got.OnAC {
		t.Errorf("OnAC = true, se esperaba false")
	}
	if got.IsCharging {
		t.Errorf("IsCharging = true, se esperaba false")
	}
	if got.BatteryPct != 39 {
		t.Errorf("BatteryPct = %d, se esperaba 39", got.BatteryPct)
	}
	if got.AmperageMA != -360 {
		t.Errorf("AmperageMA = %d, se esperaba -360", got.AmperageMA)
	}
	if got.BatteryPowerMW != -4098 {
		t.Errorf("BatteryPowerMW = %d, se esperaba -4098", got.BatteryPowerMW)
	}
	if got.SystemLoadMW != 4098 {
		t.Errorf("SystemLoadMW = %d, se esperaba 4098", got.SystemLoadMW)
	}
	if got.TimeRemainingMin == nil {
		t.Fatalf("TimeRemainingMin = nil, se esperaba puntero a 219")
	}
	if *got.TimeRemainingMin != 219 {
		t.Errorf("*TimeRemainingMin = %d, se esperaba 219", *got.TimeRemainingMin)
	}
}

// batteryExternalFalseWithWallInput simulates a battery plist where
// ExternalConnected is explicitly false while the adapter still reports a
// positive IPDInputPower. OnAC must follow ExternalConnected, not WallInput.
const batteryExternalFalseWithWallInput = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<array>
  <dict>
    <key>ExternalConnected</key>
    <false/>
    <key>PowerDistribution</key>
    <dict>
      <key>IPDInputPower</key>
      <integer>10000</integer>
    </dict>
  </dict>
</array>
</plist>`

// TestExternalConnectedFalseOverridesWallInput verifies that AC presence is
// driven by ExternalConnected when the key is present, even if the adapter
// reports positive input power.
func TestExternalConnectedFalseOverridesWallInput(t *testing.T) {
	got, err := ParseSmartBatteryPlist([]byte(batteryExternalFalseWithWallInput))
	if err != nil {
		t.Fatalf("ParseSmartBatteryPlist(ExternalConnected=false, wall>0) devolvió error: %v", err)
	}
	if got.WallInputMW != 10000 {
		t.Errorf("WallInputMW = %d, se esperaba 10000", got.WallInputMW)
	}
	if got.OnAC {
		t.Errorf("OnAC = true, se esperaba false (ExternalConnected debe prevalecer sobre WallInput)")
	}
}

// TestParseGPUIdleFixture validates generic IOAccelerator parsing and
// conversion from bytes to MiB.
func TestParseGPUIdleFixture(t *testing.T) {
	raw := readFixture(t, "m4_air_gpu_idle.plist")

	got, err := ParseIOAcceleratorPlist(raw)
	if err != nil {
		t.Fatalf("ParseIOAcceleratorPlist(gpu_idle) devolvió error inesperado: %v", err)
	}

	if got.GPUClass != "AGXAcceleratorG16G" {
		t.Errorf("GPUClass = %q, se esperaba %q", got.GPUClass, "AGXAcceleratorG16G")
	}
	if got.GPUDeviceUtilPct != 24 {
		t.Errorf("GPUDeviceUtilPct = %d, se esperaba 24", got.GPUDeviceUtilPct)
	}
	// The fixture reports 704217088 bytes. MiB = bytes / 1024 / 1024 = 671.59,
	// so the correct integer value is 671. The value 704 is only obtained by
	// dividing by 10^6 (decimal MB), which contradicts the *MiB field and the
	// package README.
	if got.GPUInUseMemMiB != 671 {
		t.Errorf("GPUInUseMemMiB = %d, se esperaba 671 (704217088 bytes / 1024 / 1024)", got.GPUInUseMemMiB)
	}
}

// TestIdleGPU24PctDoesNotTriggerFalsePositive verifies that idle utilization
// (≈24%) is never interpreted as sustained game load, even after 10 consecutive
// ticks.
func TestIdleGPU24PctDoesNotTriggerFalsePositive(t *testing.T) {
	battRaw := readFixture(t, "m4_air_battery_charging.plist")
	gpuRaw := readFixture(t, "m4_air_gpu_idle.plist")

	snap, err := BuildSnapshotFromPlists(battRaw, gpuRaw)
	if err != nil {
		t.Fatalf("BuildSnapshotFromPlists(charging, gpu_idle) devolvió error: %v", err)
	}

	ev := NewEvaluator(DefaultConfig())
	for tick := 1; tick <= 10; tick++ {
		state := ev.Evaluate(snap)
		if state == StatePausedLoad {
			t.Fatalf("tick %d: Evaluate = %q, se esperaba %q (falso positivo de carga con GPU al %d%%)",
				tick, state, StateFolding, snap.GPUDeviceUtilPct)
		}
		if state != StateFolding {
			t.Fatalf("tick %d: Evaluate = %q, se esperaba %q", tick, state, StateFolding)
		}
	}
}

// TestDischarging39PctPausesByReserve verifies that at 39% battery and without
// AC power, folding is paused due to the reserve threshold (default MinBatteryPct: 50).
func TestDischarging39PctPausesByReserve(t *testing.T) {
	battRaw := readFixture(t, "m4_air_battery_discharging.plist")
	gpuRaw := readFixture(t, "m4_air_gpu_idle.plist")

	snap, err := BuildSnapshotFromPlists(battRaw, gpuRaw)
	if err != nil {
		t.Fatalf("BuildSnapshotFromPlists(discharging, gpu_idle) devolvió error: %v", err)
	}

	state := NewEvaluator(DefaultConfig()).Evaluate(snap)
	if state != StatePausedBattery {
		t.Fatalf("Evaluate = %q, se esperaba %q (batería al %d%%, mínimo %d%%)",
			state, StatePausedBattery, snap.BatteryPct, DefaultConfig().MinBatteryPct)
	}
}

// TestDesktopMacWithoutBattery validates the Mac mini/Studio case: without a
// battery, AC is assumed and the state must be StateFolding.
func TestDesktopMacWithoutBattery(t *testing.T) {
	gpuRaw := readFixture(t, "m4_air_gpu_idle.plist")

	snap, err := BuildSnapshotFromPlists([]byte(emptyBatteryPlist), gpuRaw)
	if err != nil {
		t.Fatalf("BuildSnapshotFromPlists(sin batería, gpu_idle) devolvió error: %v", err)
	}

	if snap.HasBattery {
		t.Errorf("HasBattery = true, se esperaba false en un equipo de escritorio")
	}
	if !snap.OnAC {
		t.Errorf("OnAC = false, se esperaba true en un equipo sin batería")
	}

	state := NewEvaluator(DefaultConfig()).Evaluate(snap)
	if state != StateFolding {
		t.Fatalf("Evaluate = %q, se esperaba %q en un equipo sin batería", state, StateFolding)
	}
}

// FuzzBuildSnapshotFromPlists subjects the plist parser to arbitrary and
// truncated input. Decode errors are acceptable; any panic is a failure.
func FuzzBuildSnapshotFromPlists(f *testing.F) {
	charging := readFixture(f, "m4_air_battery_charging.plist")
	discharging := readFixture(f, "m4_air_battery_discharging.plist")
	gpuIdle := readFixture(f, "m4_air_gpu_idle.plist")

	f.Add(charging, gpuIdle)
	f.Add(discharging, gpuIdle)
	f.Add([]byte(emptyBatteryPlist), gpuIdle)
	f.Add([]byte(emptyBatteryPlist), []byte(emptyBatteryPlist))
	f.Add([]byte{}, []byte{})
	f.Add([]byte("<plist"), gpuIdle)

	f.Fuzz(func(t *testing.T, battRaw, gpuRaw []byte) {
		// The error is not asserted: corrupt or truncated input should fail with
		// an error, not panic.
		snap, err := BuildSnapshotFromPlists(battRaw, gpuRaw)
		if err != nil {
			return
		}

		// A well-formed snapshot must not cause the evaluator to panic either.
		_ = NewEvaluator(DefaultConfig()).Evaluate(snap)
	})
}
