//go:build linux && cgo && nvml_integration

package gpu

import (
	"os"
	"testing"
)

// TestNvidiaPowerLimitRoundTrip exercises the real NVML power-limit path end
// to end: lower the limit to the device minimum, then restore the default and
// verify both through GetTelemetry. It changes the GPU state, so it is opt-in:
//
//	sudo go test -tags nvml_integration -run TestNvidiaPowerLimitRoundTrip -v ./internal/gpu
func TestNvidiaPowerLimitRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("nvml_integration requires root to change the power limit (run with sudo)")
	}

	p := &NvidiaProvider{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		// Fail-safe: whatever happens above, leave the GPU at its default TDP.
		if err := p.RestoreDefaultPowerLimit(); err != nil {
			t.Errorf("cleanup RestoreDefaultPowerLimit: %v", err)
		}
		if err := p.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	before, err := p.GetTelemetry()
	if err != nil {
		t.Fatalf("GetTelemetry: %v", err)
	}
	t.Logf("%s: limit %dW, default %dW, range [%dW, %dW]",
		before.Name, before.PowerLimitW, before.DefaultPowerLimitW, before.MinPowerLimitW, before.MaxPowerLimitW)
	if before.MinPowerLimitW >= before.DefaultPowerLimitW {
		t.Skipf("no room to lower the limit: min %dW, default %dW", before.MinPowerLimitW, before.DefaultPowerLimitW)
	}

	eco := before.MinPowerLimitW
	if err := p.SetPowerLimit(eco); err != nil {
		t.Fatalf("SetPowerLimit(%d): %v", eco, err)
	}
	lowered, err := p.GetTelemetry()
	if err != nil {
		t.Fatalf("GetTelemetry after SetPowerLimit: %v", err)
	}
	if lowered.PowerLimitW != eco {
		t.Fatalf("after SetPowerLimit(%d): limit is %dW", eco, lowered.PowerLimitW)
	}

	if err := p.RestoreDefaultPowerLimit(); err != nil {
		t.Fatalf("RestoreDefaultPowerLimit: %v", err)
	}
	restored, err := p.GetTelemetry()
	if err != nil {
		t.Fatalf("GetTelemetry after RestoreDefaultPowerLimit: %v", err)
	}
	want := min(max(restored.DefaultPowerLimitW, restored.MinPowerLimitW), restored.MaxPowerLimitW)
	if restored.PowerLimitW != want {
		t.Fatalf("after RestoreDefaultPowerLimit: limit is %dW, want %dW", restored.PowerLimitW, want)
	}
}
