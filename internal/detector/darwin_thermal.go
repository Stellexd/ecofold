//go:build darwin

package detector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// thermalPressureKey is the notify(3) key macOS posts its thermal pressure
// level on. Reading it needs no privileges.
const thermalPressureKey = "com.apple.system.thermalpressurelevel"

// fetchThermalTimeout bounds one notifyutil run, which takes a few
// milliseconds.
const fetchThermalTimeout = 1 * time.Second

// maxThermalBytes bounds the notifyutil output that is read. The real line is
// under 64 bytes.
const maxThermalBytes = 4096

// maxThermalLevel is the highest level macOS defines (sleeping). Larger
// values are rejected as garbage.
const maxThermalLevel = 4

// FetchThermalPressureContext runs `notifyutil -g
// com.apple.system.thermalpressurelevel` and returns the level (see
// ThermalPressureSerious). It is only available on darwin.
func FetchThermalPressureContext(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchThermalTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/notifyutil", "-g", thermalPressureKey)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("notifyutil: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("notifyutil: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxThermalBytes+1))
	if len(out) > maxThermalBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("notifyutil: output exceeds maximum size of %d bytes", maxThermalBytes)
	}
	if err := cmd.Wait(); err != nil {
		return 0, fmt.Errorf("notifyutil: %w", err)
	}
	if readErr != nil {
		return 0, fmt.Errorf("notifyutil: %w", readErr)
	}
	return ParseThermalPressure(out)
}

// ParseThermalPressure parses notifyutil -g output, for example
// "com.apple.system.thermalpressurelevel 2\n".
func ParseThermalPressure(raw []byte) (int, error) {
	fields := strings.Fields(string(bytes.TrimSpace(raw)))
	if len(fields) != 2 || fields[0] != thermalPressureKey {
		return 0, fmt.Errorf("notifyutil: unexpected output %q", raw)
	}
	level, err := strconv.Atoi(fields[1])
	if err != nil || level < 0 || level > maxThermalLevel {
		return 0, fmt.Errorf("notifyutil: unexpected level %q", fields[1])
	}
	return level, nil
}
