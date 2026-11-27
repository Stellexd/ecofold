//go:build darwin

package detector

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// fakeProcs is a process seam: a fixed listing plus executable names.
type fakeProcs struct {
	procs     []procEntry
	listErr   error
	exec      map[int32]string
	lists     int
	execCalls []int32
}

func (f *fakeProcs) list() ([]procEntry, error) {
	f.lists++
	return f.procs, f.listErr
}

func (f *fakeProcs) execName(pid int32) (string, error) {
	f.execCalls = append(f.execCalls, pid)
	if name, ok := f.exec[pid]; ok {
		return name, nil
	}
	return "", syscall.ESRCH // the process exited
}

func pauseCollector(t *testing.T, f *fakeProcs, list string) *DarwinCollector {
	t.Helper()
	patterns, err := ParsePauseProcesses(list)
	if err != nil {
		t.Fatal(err)
	}
	c := &DarwinCollector{listProcs: f.list, execName: f.execName}
	c.SetPauseProcesses(patterns)
	return c
}

func TestDarwinPauseListMatches(t *testing.T) {
	f := &fakeProcs{procs: []procEntry{
		{1, "launchd"}, {500, "Safari"}, {501, "safari"}, {502, "OBS"},
		{503, "blender"}, {504, "com.apple.WebKi"}, {505, "Finder"},
	}}
	c := pauseCollector(t, f, "SAFARI, obs*, blend*")
	got := c.userPauseMatches(t.Context())
	if want := []string{"blender", "obs", "safari"}; !slices.Equal(got, want) {
		t.Fatalf("coincidencias = %v, se esperaba %v", got, want)
	}
	if len(f.execCalls) != 0 {
		t.Fatalf("se leyó kern.procargs2 para nombres cortos: %v", f.execCalls)
	}
}

// TestDarwinPauseListEmptyListsNothing: without a list there is no listing.
func TestDarwinPauseListEmptyListsNothing(t *testing.T) {
	f := &fakeProcs{procs: []procEntry{{500, "Safari"}}}
	c := &DarwinCollector{listProcs: f.list, execName: f.execName}
	for range 3 {
		if got := c.userPauseMatches(t.Context()); got != nil {
			t.Fatalf("coincidencias sin lista: %v", got)
		}
	}
	c.SetPauseProcesses(nil)
	_ = c.userPauseMatches(t.Context())
	if f.lists != 0 {
		t.Fatalf("se listaron procesos %d veces sin lista", f.lists)
	}
}

// TestDarwinPauseListTruncatedComm: p_comm keeps 16 bytes. A pattern longer
// than a 16-byte comm that starts with it is checked against the executable
// name; a process that exited keeps its comm and does not match.
func TestDarwinPauseListTruncatedComm(t *testing.T) {
	f := &fakeProcs{
		procs: []procEntry{
			{600, "DaVinci Resolve "}, // trailing space trimmed: 15 bytes, never truncated
			{601, "Final Cut Camera"},
			{602, "NotificationCent"},
			{603, "Logic Pro Helper"},
		},
		exec: map[int32]string{601: "Final Cut Camera Pro", 603: "Logic Pro Helper (Renderer)"},
	}
	c := pauseCollector(t, f, "final cut camera pro, notificationcenter, logic pro helper*")
	got := c.userPauseMatches(t.Context())
	if want := []string{"final cut camera pro", "logic pro helper"}; !slices.Equal(got, want) {
		t.Fatalf("coincidencias = %v, se esperaba %v", got, want)
	}
	// 602 exited before kern.procargs2: it keeps "NotificationCent", which the
	// exact pattern does not match.
	if want := []int32{601, 602}; !slices.Equal(f.execCalls, want) {
		t.Fatalf("kern.procargs2 leído para %v, se esperaba %v", f.execCalls, want)
	}
}

// TestDarwinPauseListListingErrorIsNoMatch: a failed listing never fails
// Collect and yields no matches, as on Linux.
func TestDarwinPauseListListingErrorIsNoMatch(t *testing.T) {
	f := &fakeProcs{listErr: errors.New("sysctl: cannot allocate memory")}
	c := pauseCollector(t, f, "safari")
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist"}
	c.fetch, c.batteryEvery = io.fetch, DefaultBatteryRefreshTicks
	snap, err := c.Collect(t.Context())
	if err != nil || snap.UserPauseMatches != nil {
		t.Fatalf("Collect = %v, %v", snap.UserPauseMatches, err)
	}
}

// TestDarwinCollectorReportsPauseMatches: the matches reach the snapshot, so
// the evaluator counts the tick as load.
func TestDarwinCollectorReportsPauseMatches(t *testing.T) {
	f := &fakeProcs{procs: []procEntry{{500, "Safari"}}}
	c := pauseCollector(t, f, "safari")
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist"}
	c.fetch, c.batteryEvery = io.fetch, DefaultBatteryRefreshTicks

	e := NewEvaluator(DefaultConfig())
	var state OperatingState
	for range DefaultConfig().SustainedTicksRequired {
		snap, err := c.Collect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(snap.UserPauseMatches, []string{"safari"}) {
			t.Fatalf("UserPauseMatches = %v", snap.UserPauseMatches)
		}
		state = e.Evaluate(snap)
	}
	if state != StatePausedLoad {
		t.Fatalf("estado = %s, se esperaba %s", state, StatePausedLoad)
	}
	if f.lists != DefaultConfig().SustainedTicksRequired {
		t.Fatalf("listados = %d, se esperaba uno por Collect", f.lists)
	}
}

// TestDarwinPauseListChaos: garbage names, a huge listing and a listing full
// of matches stay bounded and never report a garbage name.
func TestDarwinPauseListChaos(t *testing.T) {
	garbage := []procEntry{
		{1, ""}, {2, "\x00"}, {3, "safari\x00junk"}, {4, "saf\xffari"}, {5, "safari\n"},
		{6, "\x1b[31msafari"}, {-1, "safari"}, {0, strings.Repeat("s", 4096)},
	}
	f := &fakeProcs{procs: garbage}
	c := pauseCollector(t, f, "safari*")
	got := c.userPauseMatches(t.Context())
	// "safari\x00junk" comes from a fake listing; real p_comm is cut at the
	// first NUL by commString. "safari\n" trims to "safari".
	for _, name := range got {
		if cleanProcName(name) != name || !strings.HasPrefix(name, "safari") {
			t.Fatalf("nombre basura reportado: %q", name)
		}
	}

	huge := make([]procEntry, maxProcEntries+10)
	for i := range huge {
		huge[i] = procEntry{int32(i + 1), fmt.Sprintf("app%d", i)}
	}
	f = &fakeProcs{procs: huge}
	c = pauseCollector(t, f, "app*")
	if got := c.userPauseMatches(t.Context()); len(got) != maxPauseMatches {
		t.Fatalf("%d coincidencias, se esperaba el tope %d", len(got), maxPauseMatches)
	}

	c = pauseCollector(t, f, "nothing-matches")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := c.userPauseMatches(ctx); got != nil {
		t.Fatalf("contexto cancelado: %v", got)
	}
}

func TestCommString(t *testing.T) {
	for _, tc := range []struct {
		raw  []byte
		want string
	}{
		{[]byte("Safari\x00\x00\x00"), "Safari"},
		{[]byte("NotificationCent\x00"), "NotificationCent"},
		{[]byte("12345678901234567"), "1234567890123456"}, // no NUL: cut at MAXCOMLEN
		{[]byte("\x00Safari"), ""},
		{[]byte("bad\x01name\x00"), ""},
		{[]byte("\xff\xfe\x00"), ""},
	} {
		if got := commString(tc.raw); got != tc.want {
			t.Fatalf("commString(%q) = %q, se esperaba %q", tc.raw, got, tc.want)
		}
	}
}

func procArgs(argc int32, rest string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(argc))
	return append(b, rest...)
}

func TestParseProcArgs(t *testing.T) {
	good := []struct {
		raw  []byte
		want string
	}{
		{procArgs(1, "/Applications/Safari.app/Contents/MacOS/Safari\x00\x00\x00Safari\x00"), "Safari"},
		{procArgs(2, "/Applications/Final Cut Camera Pro.app/Contents/MacOS/Final Cut Camera Pro\x00x"), "Final Cut Camera Pro"},
		{procArgs(0, "relative-name\x00"), "relative-name"},
	}
	for _, tc := range good {
		if got, err := parseProcArgs(tc.raw); err != nil || got != tc.want {
			t.Fatalf("parseProcArgs(%q) = %q, %v", tc.raw, got, err)
		}
	}
	bad := [][]byte{
		nil, {1, 0}, procArgs(1, ""), procArgs(1, "\x00"), procArgs(1, "/no/terminator"),
		procArgs(-1, "/bin/sh\x00"), procArgs(1, "/dir/\x00"), procArgs(1, "/bin/a\x1bb\x00"),
		procArgs(1, "/bin/\xff\xfe\x00"), procArgs(1, "/"+strings.Repeat("a", 300)+"\x00"),
	}
	for _, raw := range bad {
		if got, err := parseProcArgs(raw); err == nil {
			t.Fatalf("parseProcArgs(%q) = %q, se esperaba error", raw, got)
		}
	}
}

func FuzzParseProcArgs(f *testing.F) {
	f.Add(procArgs(1, "/bin/sh\x00sh\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		name, err := parseProcArgs(raw)
		if err == nil && (name == "" || strings.Contains(name, "/") || cleanProcName(name) != name) {
			t.Fatalf("parseProcArgs(%q) = %q", raw, name)
		}
	})
}

// TestDarwinProcsLive lists this Mac's processes and finds the test binary
// under both names.
func TestDarwinProcsLive(t *testing.T) {
	procs, err := listDarwinProcs()
	if err != nil {
		t.Fatalf("listDarwinProcs: %v", err)
	}
	self := filepath.Base(os.Args[0])
	pid := int32(os.Getpid())
	i := slices.IndexFunc(procs, func(p procEntry) bool { return p.PID == pid })
	if i < 0 {
		t.Fatalf("el proceso %d no está entre %d procesos", pid, len(procs))
	}
	if want := self[:min(len(self), maxCommLen)]; procs[i].Comm != want {
		t.Fatalf("p_comm = %q, se esperaba %q", procs[i].Comm, want)
	}
	name, err := darwinExecName(pid)
	if err != nil || name != self {
		t.Fatalf("darwinExecName = %q, %v; se esperaba %q", name, err, self)
	}
	t.Logf("%d procesos", len(procs))
}
