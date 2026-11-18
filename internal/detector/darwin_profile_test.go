//go:build darwin

package detector

import (
	"context"
	"errors"
	"testing"
)

// Core counts of the machines in the tests.
var (
	hwM4Air      = HardwareInfo{Model: "Mac16,12", PhysicalCPUs: 10, PerformanceCores: 4, EfficiencyCores: 6}
	hwM4Pro14    = HardwareInfo{Model: "Mac16,1", PhysicalCPUs: 10, PerformanceCores: 4, EfficiencyCores: 6}
	hwM4ProChip  = HardwareInfo{Model: "Mac16,8", PhysicalCPUs: 14, PerformanceCores: 10, EfficiencyCores: 4}
	hwM2MaxStdio = HardwareInfo{Model: "Mac14,13", PhysicalCPUs: 12, PerformanceCores: 8, EfficiencyCores: 4}
)

func TestParseHardwareInfo(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want HardwareInfo
	}{
		{"Mac16,12\n10\n4\n6\n", hwM4Air},
		{"Mac14,13\n12\n8\n4", hwM2MaxStdio},
		{"MacBookPro16,1\n8\n", HardwareInfo{Model: "MacBookPro16,1", PhysicalCPUs: 8}}, // Intel
	} {
		got, err := ParseHardwareInfo([]byte(tc.raw))
		if err != nil || got != tc.want {
			t.Fatalf("ParseHardwareInfo(%q) = %+v, %v; se esperaba %+v", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"", "Mac16,12", "Mac16,12\n10\n4", "Mac16,12\n10\n4\n6\n7",
		"Mac 16,12\n10\n4\n6", "\n10\n4\n6", "Mac16,12\n0\n0\n0", "Mac16,12\n-1\n4\n6",
		"Mac16,12\nten\n4\n6", "Mac16,12\n10\n8\n6", "Mac16,12\n100000\n4\n6",
	} {
		if got, err := ParseHardwareInfo([]byte(raw)); err == nil {
			t.Fatalf("ParseHardwareInfo(%q) = %+v, se esperaba error", raw, got)
		}
	}
}

func TestClassifyMacProfile(t *testing.T) {
	for _, tc := range []struct {
		model   string
		battery bool
		want    MacThermalProfile
	}{
		{"MacBookAir10,1", true, ProfileFanless}, // M1 Air
		{"Mac14,2", true, ProfileFanless},
		{"Mac14,15", true, ProfileFanless},
		{"Mac15,12", true, ProfileFanless},
		{"Mac15,13", true, ProfileFanless},
		{"Mac16,12", true, ProfileFanless},
		{"Mac16,13", true, ProfileFanless},
		{"Mac16,12", false, ProfileFanless}, // the model wins over a missing battery reading
		{"MacBookPro18,3", true, ProfileActiveLaptop},
		{"Mac14,5", true, ProfileActiveLaptop},
		{"Mac15,3", true, ProfileActiveLaptop},
		{"Mac16,1", true, ProfileActiveLaptop},
		{"Mac16,8", true, ProfileActiveLaptop},
		{"Mac14,13", false, ProfileDesktop}, // Mac Studio M2 Max
		{"Mac16,10", false, ProfileDesktop}, // Mac mini M4
		{"Macmini9,1", false, ProfileDesktop},
		{"iMac24,1", false, ProfileDesktop},
		{"", false, ProfileDesktop},
		{"Mac99,1", true, ProfileFanless}, // unknown laptop: conservative
		{"", true, ProfileFanless},
	} {
		if got := ClassifyMacProfile(tc.model, tc.battery); got != tc.want {
			t.Fatalf("ClassifyMacProfile(%q, %v) = %s, se esperaba %s", tc.model, tc.battery, got, tc.want)
		}
	}
}

// TestDarwinProfile covers the three classes end to end: classification, the
// CPU budget on AC, on battery and under moderate pressure, and the thermal
// level the evaluator pauses at.
func TestDarwinProfile(t *testing.T) {
	type budget struct{ ac, battery, acModerate int }
	for _, tc := range []struct {
		name       string
		hw         HardwareInfo
		battery    bool
		profile    MacThermalProfile
		budget     budget
		pauseLevel int
	}{
		// M4 Air: 1 thread kept the CPU at or below 70 °C; it pauses at level 1.
		{"MacBook Air M4", hwM4Air, true, ProfileFanless, budget{1, 1, 1}, 1},
		// M4 MacBook Pro 14": 50 % (5 of 10) on AC, 2 on battery.
		{"MacBook Pro M4", hwM4Pro14, true, ProfileActiveLaptop, budget{5, 2, 1}, 2},
		// M4 Pro: the 4 E-cores bound the AC budget.
		{"MacBook Pro M4 Pro", hwM4ProChip, true, ProfileActiveLaptop, budget{4, 2, 1}, 2},
		// Mac Studio M2 Max: all but two cores.
		{"Mac Studio M2 Max", hwM2MaxStdio, false, ProfileDesktop, budget{10, 10, 6}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ClassifyMacProfile(tc.hw.Model, tc.battery)
			if p != tc.profile {
				t.Fatalf("perfil = %s, se esperaba %s", p, tc.profile)
			}
			unpluggedOnAC := !tc.battery // a desktop never runs on battery
			got := budget{
				ac:         RecommendedCPUs(p, tc.hw, true, 0),
				battery:    RecommendedCPUs(p, tc.hw, unpluggedOnAC, 0),
				acModerate: RecommendedCPUs(p, tc.hw, true, 1),
			}
			if got != tc.budget {
				t.Fatalf("presupuesto = %+v, se esperaba %+v", got, tc.budget)
			}
			if lvl := ThermalPauseLevel(p); lvl != tc.pauseLevel {
				t.Fatalf("nivel de pausa = %d, se esperaba %d", lvl, tc.pauseLevel)
			}

			for level := 0; level <= 4; level++ {
				e := NewEvaluator(DefaultConfig())
				s := SystemSnapshot{HasBattery: tc.battery, OnAC: true, BatteryPct: 90, ThermalLevel: level, MacProfile: p}
				got := e.Evaluate(s)
				if paused := got == StatePausedThermal; paused != (level >= tc.pauseLevel) {
					t.Fatalf("nivel %d: estado = %s", level, got)
				}
			}
		})
	}
}

// TestRecommendedCPUsBounds: the budget is at least 1 and never above the
// physical cores, also with unknown or tiny core counts.
func TestRecommendedCPUsBounds(t *testing.T) {
	for _, p := range []MacThermalProfile{ProfileFanless, ProfileActiveLaptop, ProfileDesktop, ""} {
		for _, hw := range []HardwareInfo{{}, {PhysicalCPUs: 1}, {PhysicalCPUs: 2}, {PhysicalCPUs: 8}, hwM2MaxStdio} {
			for _, ac := range []bool{true, false} {
				for level := 0; level <= 4; level++ {
					n := RecommendedCPUs(p, hw, ac, level)
					if n < 1 || (hw.PhysicalCPUs > 0 && n > hw.PhysicalCPUs) {
						t.Fatalf("RecommendedCPUs(%s, %+v, %v, %d) = %d", p, hw, ac, level, n)
					}
				}
			}
		}
	}
}

// TestDarwinCollectorReportsProfile: the collector reads the model once and
// reports the profile and budget with every snapshot.
func TestDarwinCollectorReportsProfile(t *testing.T) {
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_discharging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)
	reads := 0
	c.hardware = func(context.Context) (HardwareInfo, error) { reads++; return hwM4Air, nil }

	for range 5 {
		snap, err := c.Collect(context.Background())
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if snap.MacProfile != ProfileFanless || snap.RecommendedCPUs != 1 {
			t.Fatalf("perfil = %s, cpus = %d", snap.MacProfile, snap.RecommendedCPUs)
		}
	}
	if reads != 1 {
		t.Fatalf("sysctl se leyó %d veces, se esperaba 1", reads)
	}
}

// TestDarwinCollectorHardwareFallback: a sysctl that keeps failing is tried
// hardwareAttempts times; meanwhile and afterwards a laptop is fanless.
func TestDarwinCollectorHardwareFallback(t *testing.T) {
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)
	reads := 0
	c.hardware = func(context.Context) (HardwareInfo, error) { reads++; return HardwareInfo{}, errors.New("boom") }

	for range hardwareAttempts + 5 {
		snap, err := c.Collect(context.Background())
		if err != nil {
			t.Fatalf("un sysctl fallido no debe fallar Collect: %v", err)
		}
		if snap.MacProfile != ProfileFanless || snap.RecommendedCPUs != 1 {
			t.Fatalf("perfil = %s, cpus = %d", snap.MacProfile, snap.RecommendedCPUs)
		}
	}
	if reads != hardwareAttempts {
		t.Fatalf("sysctl se intentó %d veces, se esperaban %d", reads, hardwareAttempts)
	}
}

// TestFetchHardwareInfoLive reads this Mac's real model.
func TestFetchHardwareInfoLive(t *testing.T) {
	hw, err := FetchHardwareInfoContext(context.Background())
	if err != nil {
		t.Fatalf("FetchHardwareInfoContext: %v", err)
	}
	if hw.Model == "" || hw.PhysicalCPUs <= 0 {
		t.Fatalf("hw = %+v", hw)
	}
	t.Logf("hw = %+v, perfil con batería = %s", hw, ClassifyMacProfile(hw.Model, true))
}
