//go:build darwin

package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// TestDaemonWithRealMocks runs the daemon against the macOS MockProvider and
// fah.MockController: no power-limit calls are attempted (the mock rejects
// them), and FAH is paused on load and resumed after the cooldown.
func TestDaemonWithRealMocks(t *testing.T) {
	ctrl := fah.NewMockController(18264)
	col := &scriptedCollector{snap: snapIdle}
	logs := &syncBuffer{}
	cfg := DefaultConfig()
	cfg.RuntimeDir = t.TempDir()
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))

	d, err := New(cfg, Deps{
		GPU:       gpu.NewProvider(),
		FAH:       ctrl,
		Collector: col,
		Evaluator: detector.NewEvaluator(detector.DefaultConfig()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if d.powerCapable {
		t.Fatal("MockProvider no debe anunciar control de potencia")
	}

	ctx := context.Background()
	status := func() fah.Status {
		st, err := ctrl.GetStatus(ctx)
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		return st
	}

	col.set(snapLoad, nil)
	for i := 0; i < sustainTicks; i++ {
		d.tick(ctx)
	}
	if !status().Paused {
		t.Fatal("FAH no quedó pausado con carga sostenida")
	}

	col.set(snapIdle, nil)
	for i := 0; i < cooldownTicks; i++ {
		d.tick(ctx)
	}
	if st := status(); st.Paused || st.State != fah.StateFolding {
		t.Fatalf("estado = %+v, se esperaba plegando tras el cooldown", st)
	}

	if err := d.failSafe(false); err != nil {
		t.Fatalf("failSafe: %v", err)
	}
	if countLines(logs.String(), "level=ERROR") != 0 {
		t.Fatalf("errores registrados con mocks:\n%s", logs.String())
	}
}

// TestDarwinCollectorHasPauseList pins the macOS contract: the collector
// lists processes, so set_pause_processes is accepted and reported.
func TestDarwinCollectorHasPauseList(t *testing.T) {
	col := detector.NewDarwinCollector()
	if _, ok := any(col).(PauseListSetter); !ok {
		t.Fatal("DarwinCollector debe implementar PauseListSetter")
	}

	cfg := DefaultConfig()
	cfg.RuntimeDir = t.TempDir()
	cfg.Logger = slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
	d, err := New(cfg, Deps{
		GPU:       gpu.NewProvider(),
		FAH:       fah.NewMockController(18264),
		Collector: col,
		Evaluator: detector.NewEvaluator(detector.DefaultConfig()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := d.exec(context.Background(), &command{kind: cmdSetPauseProcesses, patterns: []string{"blender"}}); err != nil {
		t.Fatalf("set_pause_processes: %v", err)
	}
	st := d.publish()
	if !st.PauseListSupported || len(st.PauseProcesses) != 1 || st.PauseProcesses[0] != "blender" {
		t.Fatalf("status = supported %v, lista %v", st.PauseListSupported, st.PauseProcesses)
	}
}

func TestFormatGPUApps(t *testing.T) {
	got := formatGPUApps([]detector.GPUAppUsage{{PID: 812, Name: "Safari", GPUTimePct: 31}, {PID: 9, Name: "VSCodium Helper", GPUTimePct: 2}})
	if want := "Safari[812]=31% VSCodium Helper[9]=2%"; got != want {
		t.Fatalf("formatGPUApps = %q, se esperaba %q", got, want)
	}
}
