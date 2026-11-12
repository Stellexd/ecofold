//go:build linux && cgo && nvml_integration

package detector

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// TestLinuxCollectorIgnoresFoldingCoreIntegration verifies that a running
// Folding@home core is filtered out of the GPU process list. It only reads
// telemetry and does not require root.
func TestLinuxCollectorIgnoresFoldingCoreIntegration(t *testing.T) {
	provider := gpu.NewProvider()
	if err := provider.Init(); err != nil {
		t.Skipf("GPU unavailable: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown() })

	c := NewLinuxCollector(provider)
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, proc := range snap.GPUProcesses {
		comm, err := os.ReadFile(procCommPath(proc.PID))
		if err != nil {
			// Process exited between the GPU query and the /proc read.
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(comm)))
		if strings.HasPrefix(name, "fahcore_") {
			t.Errorf("FahCore process present in GPUProcesses: PID=%d comm=%q", proc.PID, strings.TrimSpace(string(comm)))
		}
	}
}
