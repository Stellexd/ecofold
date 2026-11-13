//go:build linux && cgo && nvml_integration

package detector

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// TestLinuxCollectorIgnoresFoldingCoreIntegration exercises the real Linux
// collector against the live GPU. It skips when there are no graphics clients,
// so it does NOT verify the FahCore filter by itself; that filter is covered by
// unit tests.
func TestLinuxCollectorIgnoresFoldingCoreIntegration(t *testing.T) {
	provider := gpu.NewProvider()
	if err := provider.Init(); err != nil {
		t.Skipf("GPU unavailable: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown() })

	raw, err := provider.GetGraphicsProcesses()
	if err != nil {
		t.Fatalf("GetGraphicsProcesses: %v", err)
	}
	if len(raw) == 0 {
		t.Skip("no graphics clients")
	}

	c := NewLinuxCollector(provider)
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, proc := range snap.GPUProcesses {
		comm, err := os.ReadFile(procCommPath(proc.PID))
		if err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(comm)))
		if strings.HasPrefix(name, "fahcore_") {
			t.Errorf("FahCore process present in GPUProcesses: PID=%d comm=%q", proc.PID, strings.TrimSpace(string(comm)))
		}
	}
}
