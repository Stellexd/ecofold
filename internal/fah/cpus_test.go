package fah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// cpuState is a fake FAH v8 client that applies config commands the way
// fah-client-bastet does (App::configure, Groups::configure): each group's
// dict is merged into that group, and named groups missing from "groups" are
// deleted.
type cpuState struct {
	mu       sync.Mutex
	legacy   bool           // no groups: cpus lives in the top-level config
	groups   map[string]int // group name -> cpus
	cpus     int            // legacy top-level cpus
	ignore   bool           // drop config commands
	strict   bool           // drop commands not later than the last, in seconds (validateChange)
	last     time.Time
	commands []string
}

func (st *cpuState) snapshot() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.legacy {
		return fmt.Sprintf(`{"config":{"paused":true,"cpus":%d},"units":[]}`, st.cpus)
	}
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(st.groups)) {
		parts = append(parts, fmt.Sprintf(`%q:{"config":{"paused":true,"cpus":%d}}`, name, st.groups[name]))
	}
	return `{"info":{"version":"8.5.6","cpus":10},"groups":{` + strings.Join(parts, ",") + `},"units":[]}`
}

func (st *cpuState) apply(payload []byte) {
	var cmd struct {
		Cmd    string `json:"cmd"`
		Time   string `json:"time"`
		Config struct {
			CPUs   *int                       `json:"cpus"`
			Groups map[string]json.RawMessage `json:"groups"`
		} `json:"config"`
	}
	if json.Unmarshal(payload, &cmd) != nil || cmd.Cmd != "config" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.commands = append(st.commands, string(payload))
	if st.ignore {
		return
	}
	if st.strict {
		t, err := time.Parse(time.RFC3339, cmd.Time)
		if err != nil || !t.Truncate(time.Second).After(st.last) {
			return
		}
		st.last = t.Truncate(time.Second)
	}
	if st.legacy {
		if cmd.Config.CPUs != nil {
			st.cpus = *cmd.Config.CPUs
		}
		return
	}
	if cmd.Config.Groups == nil {
		return
	}
	for name := range st.groups {
		if _, ok := cmd.Config.Groups[name]; !ok && name != "" {
			delete(st.groups, name)
		}
	}
	for name, raw := range cmd.Config.Groups {
		var g struct {
			CPUs *int `json:"cpus"`
		}
		_ = json.Unmarshal(raw, &g)
		cur, ok := st.groups[name]
		if !ok {
			cur = 0
		}
		if g.CPUs != nil {
			cur = *g.CPUs
		}
		st.groups[name] = cur
	}
}

func (st *cpuState) serve(sc *serverConn) {
	if sc.upgrade() != nil || sc.sendText(st.snapshot()) != nil {
		return
	}
	for {
		op, payload, err := sc.readFrame()
		if err != nil {
			return
		}
		switch op {
		case opClose:
			_ = sc.send(frame(true, opClose, payload))
			return
		case opText:
			st.apply(payload)
		}
	}
}

func newCPUClient(t *testing.T, st *cpuState) *ClientV8 {
	t.Helper()
	srv := startFakeFAH(t, st.serve)
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClientV8SetCPUsKeepsOtherGroups: only the default group changes, and
// every other group is listed so FAH does not delete it.
func TestClientV8SetCPUsKeepsOtherGroups(t *testing.T) {
	st := &cpuState{groups: map[string]int{"": 4, "gpu": 2, "night": 6}}
	c := newCPUClient(t, st)
	ctx := context.Background()

	if got, err := c.GetStatus(ctx); err != nil || got.CPUs != 4 {
		t.Fatalf("GetStatus = %+v, %v", got, err)
	}
	if err := c.SetCPUs(ctx, 1); err != nil {
		t.Fatalf("SetCPUs: %v", err)
	}
	if got, err := c.GetStatus(ctx); err != nil || got.CPUs != 1 || !got.Paused {
		t.Fatalf("tras SetCPUs: %+v, %v", got, err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if want := map[string]int{"": 1, "gpu": 2, "night": 6}; !maps.Equal(st.groups, want) {
		t.Fatalf("grupos = %v, se esperaba %v", st.groups, want)
	}
	if len(st.commands) != 1 {
		t.Fatalf("comandos = %v", st.commands)
	}
	var cmd struct {
		Cmd    string `json:"cmd"`
		Time   string `json:"time"`
		Config struct {
			Groups map[string]map[string]any `json:"groups"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(st.commands[0]), &cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, cmd.Time); err != nil || cmd.Cmd != "config" {
		t.Fatalf("comando = %s", st.commands[0])
	}
	if len(cmd.Config.Groups["gpu"]) != 0 || len(cmd.Config.Groups["night"]) != 0 || cmd.Config.Groups[""]["cpus"] != 1.0 {
		t.Fatalf("grupos enviados = %v", cmd.Config.Groups)
	}
}

// TestClientV8SetCPUsLegacy: a client without groups gets the top-level form.
func TestClientV8SetCPUsLegacy(t *testing.T) {
	st := &cpuState{legacy: true, cpus: 8}
	c := newCPUClient(t, st)
	if err := c.SetCPUs(context.Background(), 2); err != nil {
		t.Fatalf("SetCPUs: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cpus != 2 || strings.Contains(st.commands[0], "groups") {
		t.Fatalf("cpus = %d, comando = %s", st.cpus, st.commands[0])
	}
}

// TestClientV8SetCPUsNotConfirmed: a client that ignores the command makes
// SetCPUs fail when ctx expires, never report success.
func TestClientV8SetCPUsNotConfirmed(t *testing.T) {
	st := &cpuState{groups: map[string]int{"": 4}, ignore: true}
	c := newCPUClient(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.SetCPUs(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, se esperaba DeadlineExceeded", err)
	}
}

// TestSetCPUsRejectsOutOfRange: invalid values never reach FAH.
func TestSetCPUsRejectsOutOfRange(t *testing.T) {
	st := &cpuState{groups: map[string]int{"": 4}}
	c := newCPUClient(t, st)
	m := NewMockController(1)
	for _, n := range []int{0, -1, MaxCPUs + 1} {
		if err := c.SetCPUs(context.Background(), n); err == nil {
			t.Fatalf("ClientV8.SetCPUs(%d) aceptado", n)
		}
		if err := m.SetCPUs(context.Background(), n); err == nil {
			t.Fatalf("MockController.SetCPUs(%d) aceptado", n)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.commands) != 0 {
		t.Fatalf("llegaron comandos: %v", st.commands)
	}
}

func TestMockControllerSetCPUs(t *testing.T) {
	m := NewMockController(18626)
	ctx := context.Background()
	if err := m.SetCPUs(ctx, 3); err != nil {
		t.Fatal(err)
	}
	_ = m.Pause(ctx)
	_ = m.Resume(ctx)
	if st, _ := m.GetStatus(ctx); st.CPUs != 3 || st.State != StateFolding {
		t.Fatalf("status = %+v", st)
	}
}

// TestClientV8BackToBackConfigCommands: FAH drops a config command whose time
// is not a later second than the previous one; the client must move the
// second command's time forward instead of losing it (seen live on
// fah-client 8.5.6 on 2026-11-18: cpus 2 then 1 within a second).
func TestClientV8BackToBackConfigCommands(t *testing.T) {
	st := &cpuState{groups: map[string]int{"": 1}, strict: true}
	c := newCPUClient(t, st)
	ctx := context.Background()
	for _, n := range []int{2, 1, 3, 1} {
		if err := c.SetCPUs(ctx, n); err != nil {
			t.Fatalf("SetCPUs(%d): %v", n, err)
		}
	}
	if got, _ := c.GetStatus(ctx); got.CPUs != 1 {
		t.Fatalf("cpus = %d", got.CPUs)
	}
}
