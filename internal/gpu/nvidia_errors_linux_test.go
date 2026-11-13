//go:build linux && cgo

package gpu

import (
	"errors"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// TestFmtErrorWrapsNoPermission verifies that NVML_ERROR_NO_PERMISSION is
// exposed as ErrNoPermission so the daemon can fall back to pause-only mode.
func TestFmtErrorWrapsNoPermission(t *testing.T) {
	if err := fmtError("SetPowerManagementLimit", nvml.ERROR_NO_PERMISSION); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("err = %v, se esperaba ErrNoPermission", err)
	}
	if err := fmtError("SetPowerManagementLimit", nvml.ERROR_UNKNOWN); errors.Is(err, ErrNoPermission) {
		t.Fatalf("err = %v no debe envolver ErrNoPermission", err)
	}
}
