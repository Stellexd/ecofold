//go:build linux

package detector

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// fakeGPU is a gpu.Provider with scripted telemetry and processes.
type fakeGPU struct {
	tel      gpu.Telemetry
	telErr   error
	procs    []gpu.ProcessInfo
	procErr  error
	initCall int
}

func (f *fakeGPU) Init() error     { f.initCall++; return nil }
func (f *fakeGPU) Shutdown() error { return nil }

func (f *fakeGPU) GetTelemetry() (gpu.Telemetry, error) {
	if f.telErr != nil {
		return gpu.Telemetry{}, f.telErr
	}
	return f.tel, nil
}

func (f *fakeGPU) GetGraphicsProcesses() ([]gpu.ProcessInfo, error) {
	if f.procErr != nil {
		return nil, f.procErr
	}
	return append([]gpu.ProcessInfo(nil), f.procs...), nil
}

func (f *fakeGPU) SetPowerLimit(int) error         { return nil }
func (f *fakeGPU) RestoreDefaultPowerLimit() error { return nil }

// procFiles builds a /proc reader from a path -> content map.
func procFiles(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		v, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(v), nil
	}
}

func collectorWith(f *fakeGPU, files map[string]string) *LinuxCollector {
	c := NewLinuxCollector(f)
	c.readFile = procFiles(files)
	return c
}

// TestLinuxCollectorIgnoresFoldingCore is the core anti-self-pause test: the
// FAH core keeps the GPU at 99% and 4 GiB, but it must be filtered out so
// folding is never paused by its own workload.
func TestLinuxCollectorIgnoresFoldingCore(t *testing.T) {
	prov := &fakeGPU{
		tel: gpu.Telemetry{Name: "NVIDIA GeForce RTX 5060", UsedVRAMMiB: 4096},
		procs: []gpu.ProcessInfo{
			{PID: 42, UsedVRAMMiB: 4096, SMUtilPct: 99, SMUtilValid: true},
		},
	}
	c := collectorWith(prov, map[string]string{
		procCommPath(42):    "FahCore_22\n",
		procCmdlinePath(42): "/usr/bin/FahCore_22\x00",
	})

	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.GPUProcesses) != 0 {
		t.Fatalf("GPUProcesses = %+v, se esperaba el FAH core filtrado", snap.GPUProcesses)
	}
	if snap.GPUDeviceUtilPct != 0 || snap.GPUInUseMemMiB != 0 {
		t.Fatalf("métricas globales contaminadas: util=%d mem=%d", snap.GPUDeviceUtilPct, snap.GPUInUseMemMiB)
	}
	if snap.HasBattery || !snap.OnAC {
		t.Fatalf("snapshot de escritorio inesperado: HasBattery=%v OnAC=%v", snap.HasBattery, snap.OnAC)
	}
	if snap.GPUClass != prov.tel.Name {
		t.Errorf("GPUClass = %q, se esperaba %q", snap.GPUClass, prov.tel.Name)
	}

	// End to end: ten ticks with only FAH must never pause.
	ev := NewEvaluator(DefaultConfig())
	for i := 0; i < 10; i++ {
		if got := ev.Evaluate(snap); got != StateFolding {
			t.Fatalf("tick %d con solo FAH: estado = %q, se esperaba %q", i+1, got, StateFolding)
		}
	}
}

// TestLinuxCollectorKeepsGameProcess verifies that a real game is kept and,
// combined with the evaluator, pauses folding after the sustained ticks.
func TestLinuxCollectorKeepsGameProcess(t *testing.T) {
	prov := &fakeGPU{
		tel: gpu.Telemetry{Name: "NVIDIA GeForce RTX 5060"},
		procs: []gpu.ProcessInfo{
			{PID: 7, UsedVRAMMiB: 900, SMUtilPct: 80, SMUtilValid: true},
			{PID: 42, UsedVRAMMiB: 3000, SMUtilPct: 99, SMUtilValid: true},
		},
	}
	c := collectorWith(prov, map[string]string{
		procCommPath(7):     "deadlock.exe\n",
		procCmdlinePath(7):  "/home/p/.steam/steamapps/common/Deadlock/game/bin/linuxsteamrt64/deadlock\x00",
		procCommPath(42):    "FahCore_22\n",
		procCmdlinePath(42): "/usr/bin/FahCore_22\x00",
	})

	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.GPUProcesses) != 1 || snap.GPUProcesses[0].PID != 7 {
		t.Fatalf("GPUProcesses = %+v, se esperaba solo el PID 7", snap.GPUProcesses)
	}

	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	for i := 0; i < cfg.SustainedTicksRequired; i++ {
		ev.Evaluate(snap)
	}
	if got := ev.Evaluate(snap); got != StatePausedLoad {
		t.Fatalf("juego activo: estado = %q, se esperaba %q", got, StatePausedLoad)
	}
}

// TestLinuxCollectorFailClosedOnUnreadableProc verifies that an unreadable
// identity is kept as an unknown process rather than silently ignored.
func TestLinuxCollectorFailClosedOnUnreadableProc(t *testing.T) {
	prov := &fakeGPU{
		tel:   gpu.Telemetry{Name: "GPU"},
		procs: []gpu.ProcessInfo{{PID: 9, SMUtilPct: 60, SMUtilValid: true}},
	}
	c := NewLinuxCollector(prov)
	c.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }

	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.GPUProcesses) != 1 {
		t.Fatalf("GPUProcesses = %+v, permiso denegado debe conservar el proceso", snap.GPUProcesses)
	}
}

// TestLinuxCollectorDropsGoneProcess verifies the NVML/proc race: a PID that
// disappears between both reads is dropped, not counted as load.
func TestLinuxCollectorDropsGoneProcess(t *testing.T) {
	prov := &fakeGPU{
		tel:   gpu.Telemetry{Name: "GPU"},
		procs: []gpu.ProcessInfo{{PID: 10, SMUtilPct: 90, SMUtilValid: true}},
	}
	c := collectorWith(prov, map[string]string{})

	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.GPUProcesses) != 0 {
		t.Fatalf("GPUProcesses = %+v, se esperaba el PID ido", snap.GPUProcesses)
	}
}

// TestLinuxCollectorMatchesNamesAndArgv0 covers the ignore rules: case
// insensitive, argv0 basename, and no accidental substring matches.
func TestLinuxCollectorMatchesNamesAndArgv0(t *testing.T) {
	cases := []struct {
		name    string
		comm    string
		argv0   string
		ignored bool
	}{
		{name: "comm exacto", comm: "gnome-shell\n", argv0: "/usr/bin/gnome-shell", ignored: true},
		{name: "comm mayúsculas", comm: "FAHCLIENT\n", argv0: "/usr/bin/FAHCLIENT", ignored: true},
		{name: "prefijo FahCore", comm: "FahCore_a8\n", argv0: "/usr/bin/FahCore_a8", ignored: true},
		{name: "argv0 distinto de comm", comm: "renamed\n", argv0: "/opt/foldingathome/fahclient", ignored: true},
		{name: "prefijo parcial no ignorado", comm: "mangohudgame\n", argv0: "/usr/bin/mangohudgame", ignored: false},
		{name: "juego", comm: "deadlock.exe\n", argv0: "/games/deadlock.exe", ignored: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := collectorWith(&fakeGPU{}, map[string]string{
				procCommPath(5):    tc.comm,
				procCmdlinePath(5): tc.argv0 + "\x00",
			})
			ignored, gone := c.isIgnored(5)
			if gone {
				t.Fatal("isIgnored devolvió gone para un proceso presente")
			}
			if ignored != tc.ignored {
				t.Errorf("isIgnored = %v, se esperaba %v (comm=%q argv0=%q)", ignored, tc.ignored, tc.comm, tc.argv0)
			}
		})
	}
}

// TestLinuxCollectorIgnoresBrowserHelpers verifies that browser GPU/renderer
// helpers do not count as games while the browser main process still can.
func TestLinuxCollectorIgnoresBrowserHelpers(t *testing.T) {
	t.Run("chrome gpu-process", func(t *testing.T) {
		c := collectorWith(&fakeGPU{}, map[string]string{
			procCommPath(11):    "chrome\n",
			procCmdlinePath(11): "/opt/google/chrome/chrome\x00--type=gpu-process\x00",
		})
		if ignored, _ := c.isIgnored(11); !ignored {
			t.Error("el helper GPU de Chrome no fue ignorado")
		}
	})
	t.Run("firefox contentproc", func(t *testing.T) {
		c := collectorWith(&fakeGPU{}, map[string]string{
			procCommPath(12):    "firefox\n",
			procCmdlinePath(12): "/usr/lib/firefox/firefox\x00-contentproc\x00-childID\x001\x00",
		})
		if ignored, _ := c.isIgnored(12); !ignored {
			t.Error("el content process de Firefox no fue ignorado")
		}
	})
	t.Run("chrome main process kept", func(t *testing.T) {
		c := collectorWith(&fakeGPU{}, map[string]string{
			procCommPath(13):    "chrome\n",
			procCmdlinePath(13): "/opt/google/chrome/chrome\x00",
		})
		if ignored, _ := c.isIgnored(13); ignored {
			t.Error("el proceso principal de Chrome no debe ignorarse")
		}
	})
}

// TestLinuxCollectorErrors verifies that provider failures and cancellation are
// surfaced, never masked as an empty (load-free) snapshot.
func TestLinuxCollectorErrors(t *testing.T) {
	boom := errors.New("boom")

	t.Run("telemetry error", func(t *testing.T) {
		c := collectorWith(&fakeGPU{telErr: boom}, nil)
		if _, err := c.Collect(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v, se esperaba %v", err, boom)
		}
	})
	t.Run("process error", func(t *testing.T) {
		c := collectorWith(&fakeGPU{procErr: boom}, nil)
		if _, err := c.Collect(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v, se esperaba %v", err, boom)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := collectorWith(&fakeGPU{}, nil)
		if _, err := c.Collect(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, se esperaba context.Canceled", err)
		}
	})
	t.Run("nil provider", func(t *testing.T) {
		c := NewLinuxCollector(nil)
		if _, err := c.Collect(context.Background()); err == nil {
			t.Fatal("nil provider no devolvió error")
		}
	})
}
