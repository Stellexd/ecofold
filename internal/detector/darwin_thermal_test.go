//go:build darwin

package detector

import (
	"context"
	"errors"
	"testing"
)

func TestParseThermalPressure(t *testing.T) {
	for raw, want := range map[string]int{
		"com.apple.system.thermalpressurelevel 0\n": 0,
		"com.apple.system.thermalpressurelevel 2":   2,
		"  com.apple.system.thermalpressurelevel 4": 4,
	} {
		got, err := ParseThermalPressure([]byte(raw))
		if err != nil || got != want {
			t.Errorf("%q = %d, %v; se esperaba %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "com.apple.system.thermalpressurelevel", "com.apple.system.thermalpressurelevel x",
		"com.apple.system.thermalpressurelevel -1", "com.apple.system.thermalpressurelevel 5",
		"com.apple.other 2", "com.apple.system.thermalpressurelevel 2 3",
	} {
		if got, err := ParseThermalPressure([]byte(raw)); err == nil {
			t.Errorf("%q aceptado como %d", raw, got)
		}
	}
}

// TestDarwinCollectorReadsThermalPressure: the level reaches the snapshot,
// and a failing read degrades to the last level and then to 0 without failing
// Collect.
func TestDarwinCollectorReadsThermalPressure(t *testing.T) {
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_discharging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)
	level, fail := 2, error(nil)
	c.thermal = func(context.Context) (int, error) { return level, fail }
	ctx := context.Background()

	snap, err := c.Collect(ctx)
	if err != nil || snap.ThermalLevel != 2 {
		t.Fatalf("nivel = %d, err = %v", snap.ThermalLevel, err)
	}

	fail = errors.New("notifyutil: exit status 1")
	for i := 1; i < thermalStaleTicks; i++ {
		snap, err = c.Collect(ctx)
		if err != nil || snap.ThermalLevel != 2 {
			t.Fatalf("fallo %d: nivel = %d, err = %v", i, snap.ThermalLevel, err)
		}
	}
	if snap, err = c.Collect(ctx); err != nil || snap.ThermalLevel != 0 {
		t.Fatalf("tras %d fallos: nivel = %d, err = %v", thermalStaleTicks, snap.ThermalLevel, err)
	}

	level, fail = 3, nil
	if snap, err = c.Collect(ctx); err != nil || snap.ThermalLevel != 3 {
		t.Fatalf("recuperado: nivel = %d, err = %v", snap.ThermalLevel, err)
	}
}

// TestDarwinCollectorWithoutThermalSeam: a collector built without the seam
// (older tests, fuzzers) reports 0.
func TestDarwinCollectorWithoutThermalSeam(t *testing.T) {
	c := darwinCollectorWith(&scriptedIOReg{tb: t, battery: "m4_air_battery_discharging.plist", gpu: "m4_air_gpu_idle.plist"})
	if snap, err := c.Collect(context.Background()); err != nil || snap.ThermalLevel != 0 {
		t.Fatalf("nivel = %d, err = %v", snap.ThermalLevel, err)
	}
}

// TestFetchThermalPressureLive runs the real notifyutil.
func TestFetchThermalPressureLive(t *testing.T) {
	level, err := FetchThermalPressureContext(context.Background())
	if err != nil {
		t.Skipf("notifyutil no disponible: %v", err)
	}
	if level < 0 || level > maxThermalLevel {
		t.Fatalf("nivel = %d", level)
	}
}
