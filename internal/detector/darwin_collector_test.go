//go:build darwin

package detector

import (
	"context"
	"errors"
	"testing"
)

// scriptedIOReg is a fetch seam that serves fixtures per class and counts
// calls.
type scriptedIOReg struct {
	tb      testing.TB
	battery string
	gpu     string
	battErr error
	gpuErr  error
	calls   map[string]int
}

func (s *scriptedIOReg) fetch(ctx context.Context, class string) ([]byte, error) {
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[class]++
	switch class {
	case "AppleSmartBattery":
		if s.battErr != nil {
			return nil, s.battErr
		}
		return readFixture(s.tb, s.battery), nil
	case "IOAccelerator":
		if s.gpuErr != nil {
			return nil, s.gpuErr
		}
		return readFixture(s.tb, s.gpu), nil
	}
	s.tb.Fatalf("clase ioreg inesperada %q", class)
	return nil, nil
}

func darwinCollectorWith(s *scriptedIOReg) *DarwinCollector {
	return &DarwinCollector{fetch: s.fetch, batteryEvery: DefaultBatteryRefreshTicks}
}

// TestDarwinCollectorBuildsSnapshot verifies that a collected snapshot matches
// the one built directly from the same fixtures.
func TestDarwinCollectorBuildsSnapshot(t *testing.T) {
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_discharging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)

	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect devolvió error inesperado: %v", err)
	}
	want, err := BuildSnapshotFromPlists(
		readFixture(t, "m4_air_battery_discharging.plist"),
		readFixture(t, "m4_air_gpu_idle.plist"),
	)
	if err != nil {
		t.Fatalf("BuildSnapshotFromPlists: %v", err)
	}
	if got.HasBattery != want.HasBattery || got.OnAC != want.OnAC ||
		got.BatteryPct != want.BatteryPct || got.GPUClass != want.GPUClass ||
		got.GPUDeviceUtilPct != want.GPUDeviceUtilPct || got.GPUInUseMemMiB != want.GPUInUseMemMiB {
		t.Fatalf("snapshot = %+v, se esperaba %+v", got, want)
	}
	if got.GPUProcesses != nil {
		t.Fatal("GPUProcesses debe ser nil en macOS para usar las métricas globales")
	}
}

// TestDarwinCollectorCachesBattery verifies that the battery is read once per
// DefaultBatteryRefreshTicks calls while the GPU is read on every call.
func TestDarwinCollectorCachesBattery(t *testing.T) {
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)

	calls := 2*DefaultBatteryRefreshTicks + 1
	for i := 0; i < calls; i++ {
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatalf("Collect #%d: %v", i, err)
		}
	}
	if got := io.calls["IOAccelerator"]; got != calls {
		t.Fatalf("lecturas de IOAccelerator = %d, se esperaban %d", got, calls)
	}
	if got := io.calls["AppleSmartBattery"]; got != 3 {
		t.Fatalf("lecturas de AppleSmartBattery = %d, se esperaban 3", got)
	}
}

// TestDarwinCollectorReturnsFetchErrors verifies that fetch errors are
// reported instead of an empty "no load" snapshot, and that a failed battery
// read is retried on the next call.
func TestDarwinCollectorReturnsFetchErrors(t *testing.T) {
	boom := errors.New("ioreg falló")

	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist", battErr: boom}
	c := darwinCollectorWith(io)
	if _, err := c.Collect(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, se esperaba el error de batería", err)
	}
	io.battErr = nil
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect tras recuperar batería: %v", err)
	}
	if got := io.calls["AppleSmartBattery"]; got != 2 {
		t.Fatalf("lecturas de AppleSmartBattery = %d, se esperaban 2 (reintento)", got)
	}

	io.gpuErr = boom
	if _, err := c.Collect(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, se esperaba el error de GPU", err)
	}
}

// TestDarwinCollectorHonorsCanceledContext verifies that a canceled context
// stops collection before running ioreg.
func TestDarwinCollectorHonorsCanceledContext(t *testing.T) {
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, se esperaba context.Canceled", err)
	}
	if len(io.calls) != 0 {
		t.Fatalf("ioreg se ejecutó %v veces con el contexto cancelado", io.calls)
	}
}
