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

// hardwareOIDs are the sysctl names read, in this order. hw.perflevel0 is the
// performance cluster and hw.perflevel1 the efficiency cluster on Apple
// Silicon; Intel Macs have neither.
var hardwareOIDs = []string{"hw.model", "hw.physicalcpu", "hw.perflevel0.physicalcpu", "hw.perflevel1.physicalcpu"}

// fetchHardwareTimeout bounds one sysctl run, which takes a few
// milliseconds.
const fetchHardwareTimeout = 1 * time.Second

// maxHardwareBytes bounds the sysctl output that is read. The real output is
// under 32 bytes.
const maxHardwareBytes = 4096

// maxPhysicalCPUs rejects garbage core counts.
const maxPhysicalCPUs = 1024

// HardwareInfo is the Mac's model identifier and core counts. Zero counts
// mean unknown.
type HardwareInfo struct {
	// Model is hw.model, for example "Mac16,12" (MacBook Air M4) or
	// "MacBookPro18,3".
	Model string
	// PhysicalCPUs is hw.physicalcpu.
	PhysicalCPUs int
	// PerformanceCores and EfficiencyCores are hw.perflevel0.physicalcpu and
	// hw.perflevel1.physicalcpu; 0 on Intel Macs.
	PerformanceCores int
	EfficiencyCores  int
}

// fanlessModels are the Apple Silicon MacBook Air identifiers that do not
// start with "MacBookAir" (M2 and later). The M1 Air is MacBookAir10,1.
var fanlessModels = map[string]bool{
	"Mac14,2": true, "Mac14,15": true, // M2 13", 15"
	"Mac15,12": true, "Mac15,13": true, // M3 13", 15"
	"Mac16,12": true, "Mac16,13": true, // M4 13", 15"
}

// activeLaptopModels are the Apple Silicon MacBook Pro identifiers that do
// not start with "MacBookPro" (M2 Pro/Max and later). The M1 models are
// MacBookPro17,1 and MacBookPro18,x.
var activeLaptopModels = map[string]bool{
	"Mac14,5": true, "Mac14,6": true, "Mac14,7": true, "Mac14,9": true, "Mac14,10": true, // M2
	"Mac15,3": true, "Mac15,6": true, "Mac15,7": true, "Mac15,8": true, "Mac15,9": true, // M3
	"Mac15,10": true, "Mac15,11": true,
	"Mac16,1": true, "Mac16,5": true, "Mac16,6": true, "Mac16,7": true, "Mac16,8": true, // M4
}

// ClassifyMacProfile returns the cooling class of a Mac from its model
// identifier and whether it has a battery:
//
//   - a known MacBook Air is ProfileFanless;
//   - a Mac without a battery is ProfileDesktop;
//   - a laptop known to be a MacBook Pro is ProfileActiveLaptop;
//   - any other laptop (an unknown or unreadable model) is ProfileFanless,
//     the conservative choice.
func ClassifyMacProfile(model string, hasBattery bool) MacThermalProfile {
	switch {
	case strings.HasPrefix(model, "MacBookAir") || fanlessModels[model]:
		return ProfileFanless
	case !hasBattery:
		return ProfileDesktop
	case strings.HasPrefix(model, "MacBookPro") || activeLaptopModels[model]:
		return ProfileActiveLaptop
	default:
		return ProfileFanless
	}
}

// RecommendedCPUs returns how many CPU threads Folding@home may use. It never
// returns less than 1 or more than the physical cores when they are known.
//
//   - ProfileFanless: 1, on AC and on battery. On the MacBook Air M4 one
//     thread kept the CPU at or below 70 °C; 2 threads reached 90 °C in
//     ~35 s and 4 threads 106 °C in 5-10 s.
//   - ProfileActiveLaptop: on AC max(2, min(E-cores, physical/2)), on
//     battery max(1, min(2, E-cores/2)); 1 at thermal level 1.
//   - ProfileDesktop: max(2, physical-2), leaving two cores for the user;
//     max(2, physical/2) at thermal level 1.
//
// The pause at higher levels is the evaluator's (ThermalPauseLevel).
func RecommendedCPUs(profile MacThermalProfile, hw HardwareInfo, onAC bool, thermalLevel int) int {
	physical, ecores := hw.PhysicalCPUs, hw.EfficiencyCores
	moderate := thermalLevel >= ThermalPressureModerate
	n := 1
	switch profile {
	case ProfileActiveLaptop:
		switch {
		case moderate:
			n = 1
		case onAC:
			n = max(2, min(ecores, physical/2))
		default:
			n = max(1, min(2, ecores/2))
		}
	case ProfileDesktop:
		if moderate {
			n = max(2, physical/2)
		} else {
			n = max(2, physical-2)
		}
	}
	if physical > 0 {
		n = min(n, physical)
	}
	return max(n, 1)
}

// FetchHardwareInfoContext runs `sysctl -n hw.model hw.physicalcpu
// hw.perflevel0.physicalcpu hw.perflevel1.physicalcpu` and parses it. It
// needs no privileges and is only available on darwin.
func FetchHardwareInfoContext(ctx context.Context) (HardwareInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchHardwareTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/sbin/sysctl", append([]string{"-n"}, hardwareOIDs...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return HardwareInfo{}, fmt.Errorf("sysctl: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return HardwareInfo{}, fmt.Errorf("sysctl: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxHardwareBytes+1))
	if len(out) > maxHardwareBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return HardwareInfo{}, fmt.Errorf("sysctl: output exceeds maximum size of %d bytes", maxHardwareBytes)
	}
	// An unknown name (hw.perflevel* on Intel) makes sysctl exit 1 after
	// printing the others, so the exit status alone is not a failure.
	waitErr := cmd.Wait()
	if readErr != nil {
		return HardwareInfo{}, fmt.Errorf("sysctl: %w", readErr)
	}
	if ctx.Err() != nil {
		return HardwareInfo{}, fmt.Errorf("sysctl: %w", ctx.Err())
	}
	hw, err := ParseHardwareInfo(out)
	if err != nil && waitErr != nil {
		return HardwareInfo{}, fmt.Errorf("sysctl: %w (%v)", err, waitErr)
	}
	return hw, err
}

// ParseHardwareInfo parses the output of FetchHardwareInfoContext: one value
// per line in hardwareOIDs order. Four lines are a full reading; two lines
// are a Mac without performance levels (Intel), whose core split stays 0.
func ParseHardwareInfo(raw []byte) (HardwareInfo, error) {
	lines := strings.Split(string(bytes.TrimSpace(raw)), "\n")
	if len(lines) != 2 && len(lines) != len(hardwareOIDs) {
		return HardwareInfo{}, fmt.Errorf("sysctl: unexpected output %q", raw)
	}
	model := strings.TrimSpace(lines[0])
	if model == "" || len(model) > 64 || strings.ContainsFunc(model, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return HardwareInfo{}, fmt.Errorf("sysctl: unexpected model %q", model)
	}
	counts := make([]int, len(lines)-1)
	for i, l := range lines[1:] {
		n, err := strconv.Atoi(strings.TrimSpace(l))
		if err != nil || n < 0 || n > maxPhysicalCPUs {
			return HardwareInfo{}, fmt.Errorf("sysctl: unexpected %s %q", hardwareOIDs[i+1], l)
		}
		counts[i] = n
	}
	hw := HardwareInfo{Model: model, PhysicalCPUs: counts[0]}
	if hw.PhysicalCPUs == 0 {
		return HardwareInfo{}, fmt.Errorf("sysctl: hw.physicalcpu is 0")
	}
	if len(counts) == 3 {
		hw.PerformanceCores, hw.EfficiencyCores = counts[1], counts[2]
		if hw.PerformanceCores+hw.EfficiencyCores > hw.PhysicalCPUs {
			return HardwareInfo{}, fmt.Errorf("sysctl: %d+%d cores exceed hw.physicalcpu %d",
				hw.PerformanceCores, hw.EfficiencyCores, hw.PhysicalCPUs)
		}
	}
	return hw, nil
}
