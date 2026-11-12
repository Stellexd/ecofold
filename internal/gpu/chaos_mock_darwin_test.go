//go:build darwin

package gpu

import (
	"sync"
	"testing"
)

// TestChaosMockProviderLifecycleRace hammers Init/Shutdown against every
// read path. Under -race it must report no data race, and every call must
// return either valid data or the "not initialized" error, never a mix
// (for example an empty name with a nil error).
func TestChaosMockProviderLifecycleRace(t *testing.T) {
	p := &MockProvider{}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				switch (w + i) % 6 {
				case 0:
					_ = p.Init()
				case 1:
					_ = p.Shutdown()
				case 2:
					if tel, err := p.GetTelemetry(); err == nil && (tel.Name == "" || tel.MaxPowerLimitW != 145) {
						t.Errorf("torn telemetry with nil error: %+v", tel)
					}
				case 3:
					if procs, err := p.GetGraphicsProcesses(); err == nil && procs == nil {
						t.Error("nil process slice with nil error")
					}
				case 4:
					if p.SetPowerLimit(130) == nil {
						t.Error("SetPowerLimit succeeded on a device without power-limit support")
					}
				default:
					if p.RestoreDefaultPowerLimit() == nil {
						t.Error("RestoreDefaultPowerLimit succeeded on a device without power-limit support")
					}
				}
			}
		}()
	}
	wg.Wait()
}
