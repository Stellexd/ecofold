//go:build darwin

package gpu

import "testing"

// Compile-time check: MockProvider must satisfy Provider.
var _ Provider = (*MockProvider)(nil)

func TestMockProviderContract(t *testing.T) {
	p := NewProvider()
	mp, ok := p.(*MockProvider)
	if !ok {
		t.Fatalf("NewProvider did not return a *MockProvider")
	}

	if err := mp.Init(); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	tel, err := mp.GetTelemetry()
	if err != nil {
		t.Fatalf("GetTelemetry() failed: %v", err)
	}
	if tel.SupportsPowerLimit {
		t.Fatalf("mock device should report SupportsPowerLimit=false")
	}
	if tel.MinPowerLimitW != 123 {
		t.Fatalf("MinPowerLimitW = %d, want 123", tel.MinPowerLimitW)
	}
	if tel.MaxPowerLimitW != 145 {
		t.Fatalf("MaxPowerLimitW = %d, want 145", tel.MaxPowerLimitW)
	}
	if tel.DefaultPowerLimitW != 145 {
		t.Fatalf("DefaultPowerLimitW = %d, want 145", tel.DefaultPowerLimitW)
	}

	// SetPowerLimit must reject every value without mutating state because the
	// mock device reports SupportsPowerLimit=false.
	for _, watts := range []int{122, 123, 145, 146} {
		if err := mp.SetPowerLimit(watts); err == nil {
			t.Fatalf("SetPowerLimit(%d) expected error, got nil", watts)
		}
	}

	telAfter, err := mp.GetTelemetry()
	if err != nil {
		t.Fatalf("GetTelemetry() after SetPowerLimit failed: %v", err)
	}
	if telAfter.PowerLimitW != tel.DefaultPowerLimitW {
		t.Fatalf("SetPowerLimit mutated state: PowerLimitW = %d, want %d", telAfter.PowerLimitW, tel.DefaultPowerLimitW)
	}

	if err := mp.RestoreDefaultPowerLimit(); err == nil {
		t.Fatalf("RestoreDefaultPowerLimit() expected error, got nil")
	}

	procs, err := mp.GetGraphicsProcesses()
	if err != nil {
		t.Fatalf("GetGraphicsProcesses() failed: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("GetGraphicsProcesses() = %d processes, want 0", len(procs))
	}

	if err := mp.Shutdown(); err != nil {
		t.Fatalf("Shutdown() failed: %v", err)
	}
}

// TestMockProviderLifecycle checks that the mock mirrors NVML: every method
// fails with "not initialized" before Init and again after Shutdown (M6).
func TestMockProviderLifecycle(t *testing.T) {
	mp := &MockProvider{}

	assertNotInitialized := func(t *testing.T, stage string) {
		t.Helper()
		const want = "gpu: provider not initialized"
		if _, err := mp.GetTelemetry(); err == nil || err.Error() != want {
			t.Errorf("%s: GetTelemetry() error = %v, want %q", stage, err, want)
		}
		if err := mp.SetPowerLimit(123); err == nil || err.Error() != want {
			t.Errorf("%s: SetPowerLimit() error = %v, want %q", stage, err, want)
		}
		if err := mp.RestoreDefaultPowerLimit(); err == nil || err.Error() != want {
			t.Errorf("%s: RestoreDefaultPowerLimit() error = %v, want %q", stage, err, want)
		}
		if procs, err := mp.GetGraphicsProcesses(); err == nil || err.Error() != want || procs != nil {
			t.Errorf("%s: GetGraphicsProcesses() = %v, %v, want nil, %q", stage, procs, err, want)
		}
	}

	assertNotInitialized(t, "before Init")

	if err := mp.Init(); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}
	if _, err := mp.GetTelemetry(); err != nil {
		t.Fatalf("GetTelemetry() after Init failed: %v", err)
	}

	if err := mp.Shutdown(); err != nil {
		t.Fatalf("Shutdown() failed: %v", err)
	}
	assertNotInitialized(t, "after Shutdown")

	// Init after Shutdown must make the provider usable again.
	if err := mp.Init(); err != nil {
		t.Fatalf("second Init() failed: %v", err)
	}
	if _, err := mp.GetTelemetry(); err != nil {
		t.Fatalf("GetTelemetry() after second Init failed: %v", err)
	}
}
