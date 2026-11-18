package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
	"github.com/Stellexd/ecofold/internal/ipc"
)

// testGPU is a power-capable provider (100-145 W) that records its limit and
// whether the socket still existed when the fail-safe ran.
type testGPU struct {
	socket string

	mu              sync.Mutex
	limit           int
	restores        int
	socketAtRestore bool
}

func (g *testGPU) Init() error     { return nil }
func (g *testGPU) Shutdown() error { return nil }

func (g *testGPU) GetTelemetry() (gpu.Telemetry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return gpu.Telemetry{
		Name: "Test RTX 5060", SupportsPowerLimit: true, PowerLimitW: g.limit,
		MinPowerLimitW: 100, MaxPowerLimitW: 145, DefaultPowerLimitW: 145,
	}, nil
}

func (g *testGPU) SetPowerLimit(w int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = w
	return nil
}

func (g *testGPU) RestoreDefaultPowerLimit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = 145
	g.restores++
	_, err := os.Lstat(g.socket)
	g.socketAtRestore = err == nil
	return nil
}

func (g *testGPU) GetGraphicsProcesses() ([]gpu.ProcessInfo, error) { return nil, nil }

func (g *testGPU) state() (limit, restores int, socketAtRestore bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit, g.restores, g.socketAtRestore
}

// testCollector reports an idle machine on AC and accepts a pause list.
type testCollector struct {
	mu       sync.Mutex
	patterns []string
}

func (c *testCollector) Collect(context.Context) (detector.SystemSnapshot, error) {
	return detector.SystemSnapshot{OnAC: true}, nil
}

func (c *testCollector) SetPauseProcesses(p []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.patterns = append([]string(nil), p...)
}

func (c *testCollector) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patterns
}

type fixture struct {
	gpu    *testGPU
	fah    *fah.MockController
	col    *testCollector
	d      *daemon.Daemon
	dir    string
	socket string
}

// newFixture builds a real daemon over fakes. The directory is short because
// the socket path must fit in sun_path.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "ef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	f := &fixture{dir: dir, socket: filepath.Join(dir, "run", "ecofold.sock")}
	f.gpu = &testGPU{socket: f.socket, limit: 145}
	f.fah = fah.NewMockController(18264)
	f.col = &testCollector{}

	cfg := daemon.DefaultConfig()
	cfg.Tick = 5 * time.Millisecond
	cfg.RuntimeDir = filepath.Join(dir, "run")
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	f.d, err = daemon.New(cfg, daemon.Deps{
		GPU: f.gpu, FAH: f.fah, Collector: f.col,
		Evaluator: detector.NewEvaluator(detector.DefaultConfig()),
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return f
}

// start runs serve and returns the function that stops it and reports its
// error.
func (f *fixture) start(t *testing.T, socket string) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(ctx, logger, f.d, socket, "") }()

	deadline := time.Now().Add(3 * time.Second)
	for f.d.Status().Mode != daemon.ModeFolding {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("el daemon no llegó a folding: %+v", f.d.Status())
		}
		time.Sleep(time.Millisecond)
	}
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				err = errors.New("serve no terminó")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func call(t *testing.T, c *ipc.Client, cmd string, args any) (daemon.Status, error) {
	t.Helper()
	raw, err := c.Call(context.Background(), cmd, args)
	if err != nil {
		return daemon.Status{}, err
	}
	var st daemon.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("%s: data %s: %v", cmd, raw, err)
	}
	return st, nil
}

func wantRemote(t *testing.T, err error, code string) {
	t.Helper()
	var re *ipc.RemoteError
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, se esperaba el código %q", err, code)
	}
}

// TestServeEndToEnd drives a real daemon loop through the real socket: every
// command, the error codes, and the shutdown order.
func TestServeEndToEnd(t *testing.T) {
	f := newFixture(t)
	stop := f.start(t, f.socket)

	c, err := ipc.Dial(context.Background(), f.socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	st, err := call(t, c, ipc.CmdStatus, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != daemon.ModeFolding || st.EcoWatts != 123 || st.PowerLimitW != 123 || !st.CapApplied ||
		st.MinPowerLimitW != 100 || st.MaxPowerLimitW != 145 || st.ManualOverride != daemon.OverrideNone {
		t.Fatalf("status = %+v", st)
	}

	// The wire format is snake_case JSON with arrays, never null.
	raw, _ := c.Call(context.Background(), ipc.CmdStatus, nil)
	for _, want := range []string{`"mode":"folding"`, `"eco_watts":123`, `"manual_override":"none"`,
		`"game_sessions":[]`, `"pause_processes":[]`, `"fah":{`, `"updated_at":"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("falta %s en %s", want, raw)
		}
	}

	// set_eco_watts: validated against the device, never clamped.
	for _, w := range []int{99, 146, 0, -1} {
		_, err := call(t, c, ipc.CmdSetEcoWatts, map[string]int{"watts": w})
		wantRemote(t, err, ipc.CodeOutOfRange)
	}
	if limit, _, _ := f.gpu.state(); limit != 123 {
		t.Fatalf("un valor fuera de rango llegó a la GPU: %d W", limit)
	}
	if st, err = call(t, c, ipc.CmdSetEcoWatts, map[string]int{"watts": 110}); err != nil || st.EcoWatts != 110 {
		t.Fatalf("set_eco_watts 110: %v, %+v", err, st)
	}
	if limit, _, _ := f.gpu.state(); limit != 110 {
		t.Fatalf("límite = %d W, se esperaban 110", limit)
	}
	for _, args := range []any{nil, map[string]any{}, map[string]any{"watts": "110"}, map[string]any{"watts": 110.5},
		map[string]any{"watts": 110, "force": true}, []int{110}} {
		_, err := call(t, c, ipc.CmdSetEcoWatts, args)
		wantRemote(t, err, ipc.CodeBadRequest)
	}

	// force_pause / force_resume.
	if st, err = call(t, c, ipc.CmdForcePause, nil); err != nil {
		t.Fatalf("force_pause: %v", err)
	}
	if st.ManualOverride != daemon.OverridePause || st.Mode != daemon.ModePausedManual || !st.PausedByUs || st.PowerLimitW != 145 {
		t.Fatalf("status tras force_pause = %+v", st)
	}
	if fst, _ := f.fah.GetStatus(context.Background()); !fst.Paused {
		t.Fatal("FAH no quedó en pausa")
	}
	time.Sleep(50 * time.Millisecond) // several ticks: the evaluator must not resume
	if fst, _ := f.fah.GetStatus(context.Background()); !fst.Paused {
		t.Fatal("el loop reanudó durante la pausa manual")
	}
	if st, err = call(t, c, ipc.CmdForceResume, nil); err != nil {
		t.Fatalf("force_resume: %v", err)
	}
	if st.ManualOverride != daemon.OverrideNone || st.PausedByUs || st.PowerLimitW != 110 || st.FAH.Paused {
		t.Fatalf("status tras force_resume = %+v", st)
	}
	_, err = call(t, c, ipc.CmdForcePause, map[string]any{"x": 1})
	wantRemote(t, err, ipc.CodeBadRequest)

	// set_pause_processes.
	st, err = call(t, c, ipc.CmdSetPauseProcesses, map[string]any{"patterns": []string{"Blender", "obs*"}})
	if err != nil || strings.Join(st.PauseProcesses, "|") != "blender|obs*" {
		t.Fatalf("set_pause_processes: %v, %+v", err, st.PauseProcesses)
	}
	if got := strings.Join(f.col.get(), "|"); got != "blender|obs*" {
		t.Fatalf("collector = %q", got)
	}
	for _, args := range []any{
		map[string]any{"patterns": []string{"blender,obs"}},
		map[string]any{"patterns": []string{"FahCore_*"}},
		map[string]any{"patterns": "blender"},
		map[string]any{"patterns": nil},
		map[string]any{},
		nil,
	} {
		_, err := call(t, c, ipc.CmdSetPauseProcesses, args)
		wantRemote(t, err, ipc.CodeBadRequest)
	}
	if got := strings.Join(f.col.get(), "|"); got != "blender|obs*" {
		t.Fatalf("una lista inválida cambió el collector: %q", got)
	}
	if st, err = call(t, c, ipc.CmdSetPauseProcesses, map[string]any{"patterns": []string{}}); err != nil || len(st.PauseProcesses) != 0 {
		t.Fatalf("borrar la lista: %v, %+v", err, st.PauseProcesses)
	}

	_, err = c.Call(context.Background(), "shutdown", nil)
	wantRemote(t, err, ipc.CodeUnknownCmd)

	// Shutdown: the socket goes first, then the fail-safe restores the limit.
	if err := stop(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := os.Lstat(f.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("el socket sigue en disco tras el apagado: %v", err)
	}
	limit, restores, socketAtRestore := f.gpu.state()
	if limit != 145 || restores < 2 {
		t.Fatalf("fail-safe: límite %d W, %d restores", limit, restores)
	}
	if socketAtRestore {
		t.Fatal("el fail-safe corrió con el socket todavía abierto")
	}
	if _, err := c.Call(context.Background(), ipc.CmdStatus, nil); err == nil {
		t.Fatal("el socket respondió tras el apagado")
	}
}

// TestServeWithoutSocket: a socket that cannot be created does not stop the
// power control loop or its fail-safe.
func TestServeWithoutSocket(t *testing.T) {
	for name, path := range map[string]func(f *fixture) string{
		"ruta ocupada por un archivo": func(f *fixture) string {
			if err := os.MkdirAll(filepath.Dir(f.socket), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.socket, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			return f.socket
		},
		"ipc desactivado": func(*fixture) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			socket := path(f)
			stop := f.start(t, socket)

			if limit, _, _ := f.gpu.state(); limit != 123 {
				t.Fatalf("el loop no aplicó el tope sin IPC: %d W", limit)
			}
			if err := stop(); err != nil {
				t.Fatalf("serve: %v", err)
			}
			if limit, _, _ := f.gpu.state(); limit != 145 {
				t.Fatalf("el fail-safe no restauró el límite: %d W", limit)
			}
			if socket != "" {
				if got, err := os.ReadFile(socket); err != nil || string(got) != "keep" {
					t.Fatalf("el archivo en la ruta del socket cambió: %q, %v", got, err)
				}
			}
		})
	}
}

// TestServeRefusesSecondInstance (incident 2026-11-18): a daemon whose socket
// is held by a live daemon must exit with an error before its loop starts,
// leaving the GPU, FAH and the pause marker exactly as they were.
func TestServeRefusesSecondInstance(t *testing.T) {
	first := newFixture(t)
	first.start(t, first.socket)

	second := newFixture(t)
	runDir := filepath.Join(second.dir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(runDir, "paused-by-daemon")
	if err := os.WriteFile(marker, []byte("manual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := second.fah.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), logger, second.d, first.socket, "") }()
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve siguió corriendo con el socket ocupado")
	}
	if !errors.Is(err, ipc.ErrAlreadyRunning) {
		t.Fatalf("err = %v, se esperaba ErrAlreadyRunning", err)
	}
	if st := second.d.Status(); st.Mode != daemon.ModeStarting {
		t.Fatalf("el segundo daemon llegó a correr: %+v", st)
	}
	if limit, restores, _ := second.gpu.state(); limit != 145 || restores != 0 {
		t.Fatalf("el segundo daemon tocó la GPU: %d W, %d restauraciones", limit, restores)
	}
	if st, _ := second.fah.GetStatus(context.Background()); !st.Paused {
		t.Fatal("el segundo daemon reanudó FAH")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "manual\n" {
		t.Fatalf("el marcador cambió: %q, %v", got, err)
	}

	// The first daemon still answers.
	c, err := ipc.Dial(context.Background(), first.socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := call(t, c, ipc.CmdStatus, nil); err != nil {
		t.Fatalf("el primer daemon dejó de responder: %v", err)
	}
}

// TestPauseFAH covers the -pause-fah emergency stop.
func TestPauseFAH(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	m := fah.NewMockController(18803)
	if code := pauseFAH(logger, m); code != 0 {
		t.Fatalf("código = %d, se esperaba 0", code)
	}
	m.SetError(nil)
	if _, err := m.GetStatus(context.Background()); !errors.Is(err, fah.ErrClosed) {
		t.Fatalf("el controlador no se cerró: %v", err)
	}

	m = fah.NewMockController(18803)
	m.SetError(errors.New("connection refused"))
	if code := pauseFAH(logger, m); code == 0 {
		t.Fatal("un FAH inalcanzable debía devolver un código distinto de cero")
	}
}

// TestCommandsDuringShutdown: clients hammering the socket
// while the daemon stops never hang and never see a wrong success.
func TestCommandsDuringShutdown(t *testing.T) {
	f := newFixture(t)
	stop := f.start(t, f.socket)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := ipc.Dial(context.Background(), f.socket)
			if err != nil {
				return
			}
			defer c.Close()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := c.Call(ctx, ipc.CmdForcePause, nil)
				cancel()
				if err != nil {
					var re *ipc.RemoteError
					if errors.As(err, &re) && re.Code != ipc.CodeStopping && re.Code != ipc.CodeBusy {
						t.Errorf("código inesperado durante el apagado: %v", err)
					}
					if errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("un cliente quedó colgado durante el apagado: %v", err)
					}
					return
				}
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	wg.Wait()
	if limit, _, _ := f.gpu.state(); limit != 145 {
		t.Fatalf("límite tras el apagado = %d W", limit)
	}
}
