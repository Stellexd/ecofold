// Package gui holds the desktop client's logic without any toolkit: the IPC
// controller, the view state it produces and the tray icons. cmd/ecofold-gui
// renders a View with Fyne; everything here is testable without a display.
//
// The client is unprivileged. It never imports internal/daemon: it decodes
// the status from the wire format documented in internal/ipc/README.md.
package gui

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// Status is the daemon status as sent over the socket. Fields the client
// does not use are left out; unknown fields are ignored, so an older client
// keeps working with a newer daemon.
type Status struct {
	Mode           string `json:"mode"`
	ManualOverride string `json:"manual_override"`
	PausedByUs     bool   `json:"paused_by_us"`
	StartupHold    bool   `json:"startup_hold"`
	Degraded       bool   `json:"degraded"`

	PowerCapable       bool `json:"power_capable"`
	PowerEnabled       bool `json:"power_enabled"`
	CapApplied         bool `json:"cap_applied"`
	PowerLimitW        int  `json:"power_limit_w"`
	DefaultPowerLimitW int  `json:"default_power_limit_w"`
	MinPowerLimitW     int  `json:"min_power_limit_w"`
	MaxPowerLimitW     int  `json:"max_power_limit_w"`
	EcoWatts           int  `json:"eco_watts"`

	GPU struct {
		Name         string `json:"name"`
		TempC        int    `json:"temp_c"`
		PowerUsageW  int    `json:"power_usage_w"`
		UsedVRAMMiB  int    `json:"used_vram_mib"`
		TotalVRAMMiB int    `json:"total_vram_mib"`
		UtilPct      int    `json:"util_pct"`
		InUseMemMiB  int    `json:"in_use_mem_mib"`
	} `json:"gpu"`
	Battery struct {
		Present  bool `json:"present"`
		OnAC     bool `json:"on_ac"`
		Charging bool `json:"charging"`
		Pct      int  `json:"pct"`
	} `json:"battery"`
	FAH struct {
		Paused          bool      `json:"paused"`
		State           string    `json:"state"`
		ActiveProjectID int       `json:"active_project_id"`
		Reachable       bool      `json:"reachable"`
		ObservedAt      time.Time `json:"observed_at"`
	} `json:"fah"`

	GameSessions       []string `json:"game_sessions"`
	PauseListSupported bool     `json:"pause_list_supported"`
	PauseProcesses     []string `json:"pause_processes"`
	PauseMatches       []string `json:"pause_matches"`
	// MacProfile and RecommendedCPUs describe how the daemon limits FAH's
	// CPU threads on macOS; empty and 0 on Linux.
	MacProfile      string `json:"mac_profile"`
	RecommendedCPUs int    `json:"recommended_cpus"`
	// FoldingSeconds and ProjectsContributed count this run of the daemon.
	FoldingSeconds      int64     `json:"folding_seconds"`
	ProjectsContributed []int     `json:"projects_contributed"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// Daemon modes and overrides, as they appear in Status.
const (
	ModeStarting      = "starting"
	ModeFolding       = "folding"
	ModeFoldingEco    = "folding_eco"
	ModePausedLoad    = "paused_load"
	ModePausedBattery = "paused_battery"
	ModePausedThermal = "paused_thermal"
	ModePausedManual  = "paused_manual"
	ModeDegraded      = "degraded"

	OverridePause = "pause"

	// FAHStateIdle is Status.FAH.State when FAH is not paused but no work
	// unit is running.
	FAHStateIdle = "idle"
)

// Tone is the color family of the tray icon.
type Tone int

const (
	// ToneGray: manual pause, degraded, starting or no daemon.
	ToneGray Tone = iota
	// ToneGreen: folding.
	ToneGreen
	// ToneBlue: paused by a game or GPU load.
	ToneBlue
	// ToneAmber: paused by the battery or the thermal guard.
	ToneAmber
)

// View is everything the UI shows. It is a value: the controller hands out
// copies and never mutates one it gave away.
type View struct {
	// Connected is true when the last status request succeeded.
	Connected bool
	// Offline says why not ("Daemon not running", "Access denied"), and
	// OfflineHint what to do about it.
	Offline     string
	OfflineHint string

	// Status is the last status received. It is meaningful only while
	// Connected.
	Status Status
	// Project is the metadata of Status.FAH.ActiveProjectID once it has been
	// fetched. Until then, or when the lookup fails, it is the zero value.
	Project Project

	// Message is the result of the last user action, shown in the status
	// bar; MessageIsError marks a failure.
	Message        string
	MessageIsError bool
	MessageAt      time.Time
}

// Tone picks the tray color for the current state.
func (v View) Tone() Tone {
	if !v.Connected {
		return ToneGray
	}
	switch v.Status.Mode {
	case ModeFolding, ModeFoldingEco:
		// Green means work is being done, not that it is allowed.
		if _, stalled := v.foldingStall(); stalled {
			return ToneGray
		}
		return ToneGreen
	case ModePausedLoad:
		return ToneBlue
	case ModePausedBattery, ModePausedThermal:
		return ToneAmber
	default:
		return ToneGray
	}
}

// ModeLabel is the mode in words.
func (v View) ModeLabel() string {
	if !v.Connected {
		return "Daemon Offline"
	}
	switch v.Status.Mode {
	case ModeFolding, ModeFoldingEco:
		// The mode is the daemon's decision; what FAH does with it may differ.
		if label, stalled := v.foldingStall(); stalled {
			return label
		}
		if v.Status.Mode == ModeFoldingEco {
			return "Folding (on battery)"
		}
		return "Folding"
	case ModePausedLoad:
		return "Paused (Game)"
	case ModePausedBattery:
		return "Paused (Battery)"
	case ModePausedThermal:
		return "Paused (Thermal)"
	case ModePausedManual:
		return "Paused (Manual)"
	case ModeDegraded:
		return "Degraded"
	case ModeStarting:
		return "Starting"
	default:
		return v.Status.Mode
	}
}

// Headline is the one-line summary for the tray menu, for example
// "Folding — 123 W" or "Daemon Offline".
func (v View) Headline() string {
	if !v.Connected {
		return "Daemon Offline"
	}
	if v.Status.PowerCapable && v.Status.PowerLimitW > 0 {
		return fmt.Sprintf("%s — %d W", v.ModeLabel(), v.Status.PowerLimitW)
	}
	return v.ModeLabel()
}

// WantsResume reports whether the quick action should resume rather than
// pause: a manual pause is active, or folding is allowed but the user paused
// FAH itself.
func (v View) WantsResume() bool {
	if !v.Connected {
		return false
	}
	s := v.Status
	if s.ManualOverride == OverridePause {
		return true
	}
	folding := s.Mode == ModeFolding || s.Mode == ModeFoldingEco
	return folding && s.FAH.Reachable && s.FAH.Paused
}

// ToggleLabel is the quick action's label.
func (v View) ToggleLabel() string {
	if v.WantsResume() {
		return "Resume Folding"
	}
	return "Pause Folding"
}

// fahUnreachable reports that the daemon asked Folding@home and got no
// answer. Before the first check it is false.
func (v View) fahUnreachable() bool {
	f := v.Status.FAH
	return !f.ObservedAt.IsZero() && !f.Reachable
}

// foldingStall reports, for a mode that allows folding, why Folding@home is
// not actually running a work unit, in words.
func (v View) foldingStall() (label string, stalled bool) {
	f := v.Status.FAH
	switch {
	case v.fahUnreachable():
		return "Folding@home Not Reachable", true
	case !f.Reachable:
		return "", false // not asked yet
	case f.Paused && v.Status.PausedByUs && v.Status.StartupHold:
		// A pause from the previous run, kept until the startup hold ends.
		return "Starting Folding…", true
	case f.Paused && v.Status.PausedByUs:
		// The daemon's own pause, about to be lifted: the resume has not
		// gone through yet.
		return "Resuming Folding…", true
	case f.Paused:
		return "Paused in Folding@home", true
	case f.State == FAHStateIdle:
		return "Idle (Waiting for Work Unit)", true
	}
	return "", false
}

// ComputeTimeLabel is the time folded since the daemon started, and the
// number of projects it went to.
func (v View) ComputeTimeLabel() string {
	if !v.Connected {
		return "Session compute donated: —"
	}
	secs := v.Status.FoldingSeconds
	return fmt.Sprintf("Session compute donated: %dh %dm · Projects: %d",
		secs/3600, secs%3600/60, len(v.Status.ProjectsContributed))
}

// NewClientUUID returns a random (version 4) UUID. It identifies this
// install on the leaderboard without an account.
func NewClientUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// FAHLabel describes what the daemon last saw of Folding@home.
func (v View) FAHLabel() string {
	f := v.Status.FAH
	switch {
	case f.ObservedAt.IsZero():
		return "Folding@home: not checked yet"
	case !f.Reachable:
		return "Folding@home: not reachable (is the client running?)"
	case f.Paused && v.Status.PausedByUs:
		return "Folding@home: paused by EcoFold"
	case f.Paused:
		return "Folding@home: paused"
	case f.State == FAHStateIdle:
		return "Folding@home: idle (waiting for work unit)"
	case f.ActiveProjectID > 0:
		return fmt.Sprintf("Folding@home: running (project #%d)", f.ActiveProjectID)
	default:
		return "Folding@home: running"
	}
}

// Metric is one labelled figure of the GPU card.
type Metric struct {
	Label, Value string
}

// Metrics are the three figures of the GPU card. A system without power
// control and without VRAM figures (Apple Silicon) has no watts or
// temperature to show, only the device-wide load and memory.
func (v View) Metrics() [3]Metric {
	s, g := v.Status, v.Status.GPU
	if !s.PowerCapable && g.TotalVRAMMiB == 0 {
		return [3]Metric{
			{"GPU Load", fmt.Sprintf("%d %%", g.UtilPct)},
			{"Unified Memory", fmt.Sprintf("%d MiB", g.InUseMemMiB)},
			{"Power Mode", "Firmware Managed"},
		}
	}
	power := fmt.Sprintf("%d W", g.PowerUsageW)
	if s.PowerLimitW > 0 {
		power = fmt.Sprintf("%d W / %d W", g.PowerUsageW, s.PowerLimitW)
	}
	return [3]Metric{
		{"Power", power},
		{"Temperature", fmt.Sprintf("%d °C", g.TempC)},
		{"VRAM", fmt.Sprintf("%d / %d MiB", g.UsedVRAMMiB, g.TotalVRAMMiB)},
	}
}

// CauseLabel is the active project's research area, or empty when it is not
// known.
func (v View) CauseLabel() string {
	if v.Project.Cause == "" {
		return ""
	}
	return "Researching: " + v.Project.Cause
}

// BatteryLabel describes the battery, or is empty without one.
func (v View) BatteryLabel() string {
	b := v.Status.Battery
	if !b.Present {
		return ""
	}
	state := "on battery"
	switch {
	case b.Charging:
		state = "charging"
	case b.OnAC:
		state = "on AC"
	}
	return fmt.Sprintf("%d %%, %s", b.Pct, state)
}

// PauseListText is the pause list as the user edits it.
func (v View) PauseListText() string {
	return strings.Join(v.Status.PauseProcesses, ", ")
}

// ParsePauseList splits what the user typed, by commas or new lines, into
// patterns. The daemon validates them.
func ParsePauseList(text string) []string {
	patterns := []string{}
	for _, p := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}
