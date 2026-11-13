package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/gpu"
)

// callLog records GPU and FAH calls in one ordered list so tests can assert
// the order of actions across both dependencies.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *callLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

// count returns how many recorded calls equal call.
func (l *callLog) count(call string) int {
	n := 0
	for _, c := range l.all() {
		if c == call {
			n++
		}
	}
	return n
}

// index returns the position of the first call equal to call, or -1.
func (l *callLog) index(call string) int {
	for i, c := range l.all() {
		if c == call {
			return i
		}
	}
	return -1
}

// fakeGPU is a gpu.Provider with scripted telemetry and injectable errors.
type fakeGPU struct {
	log *callLog

	mu           sync.Mutex
	tel          gpu.Telemetry
	initErr      error
	telErr       error
	setErr       error
	restoreErr   error
	restoreBlock chan struct{} // when non-nil, Restore blocks until it is closed
}

var _ gpu.Provider = (*fakeGPU)(nil)

func newFakeGPU(log *callLog, supports bool) *fakeGPU {
	tel := gpu.Telemetry{Name: "Fake RTX 5060", SupportsPowerLimit: supports}
	if supports {
		tel.MinPowerLimitW, tel.MaxPowerLimitW, tel.DefaultPowerLimitW = 100, 145, 145
		tel.PowerLimitW = 145
	}
	return &fakeGPU{log: log, tel: tel}
}

func (g *fakeGPU) Init() error {
	g.log.add("gpu.Init")
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.initErr
}

func (g *fakeGPU) Shutdown() error {
	g.log.add("gpu.Shutdown")
	return nil
}

func (g *fakeGPU) GetTelemetry() (gpu.Telemetry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tel, g.telErr
}

func (g *fakeGPU) SetPowerLimit(watts int) error {
	g.log.add(fmt.Sprintf("gpu.Set(%d)", watts))
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.setErr
}

func (g *fakeGPU) RestoreDefaultPowerLimit() error {
	g.log.add("gpu.Restore")
	g.mu.Lock()
	block, err := g.restoreBlock, g.restoreErr
	g.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (g *fakeGPU) GetGraphicsProcesses() ([]gpu.ProcessInfo, error) {
	return nil, nil
}

func (g *fakeGPU) set(fn func(g *fakeGPU)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn(g)
}

// fakeFAH is a fah.Controller with per-method error injection.
type fakeFAH struct {
	log *callLog

	mu        sync.Mutex
	status    fah.Status
	statusErr error
	pauseErr  error
	resumeErr error
	closed    bool
}

var _ fah.Controller = (*fakeFAH)(nil)

func newFakeFAH(log *callLog) *fakeFAH {
	return &fakeFAH{log: log, status: fah.Status{ActiveProjectID: 18264, State: fah.StateFolding}}
}

func (f *fakeFAH) Pause(ctx context.Context) error {
	f.log.add("fah.Pause")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr != nil {
		return f.pauseErr
	}
	f.status = fah.Status{Paused: true, State: fah.StatePaused}
	return nil
}

func (f *fakeFAH) Resume(ctx context.Context) error {
	f.log.add("fah.Resume")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.status = fah.Status{ActiveProjectID: 18264, State: fah.StateFolding}
	return nil
}

func (f *fakeFAH) GetStatus(ctx context.Context) (fah.Status, error) {
	f.log.add("fah.Status")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return fah.Status{State: fah.StateUnknown}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeFAH) Close() error {
	f.log.add("fah.Close")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeFAH) set(fn func(f *fakeFAH)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// scriptedCollector returns the snapshot or error set by the test. It panics
// on demand to exercise the loop's recover.
type scriptedCollector struct {
	mu      sync.Mutex
	snap    detector.SystemSnapshot
	err     error
	doPanic bool
	calls   int
}

func (c *scriptedCollector) Collect(ctx context.Context) (detector.SystemSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.doPanic {
		panic("collector exploded")
	}
	return c.snap, c.err
}

func (c *scriptedCollector) set(snap detector.SystemSnapshot, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap, c.err = snap, err
}

func (c *scriptedCollector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// countingEvaluator counts Evaluate calls on a real detector.Evaluator.
type countingEvaluator struct {
	mu    sync.Mutex
	inner *detector.Evaluator
	calls int
}

func (e *countingEvaluator) Evaluate(s detector.SystemSnapshot) detector.OperatingState {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return e.inner.Evaluate(s)
}

func (e *countingEvaluator) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Snapshots shared by the tests. The evaluator uses the device-wide fields
// because GPUProcesses is nil.
var (
	snapIdle       = detector.SystemSnapshot{OnAC: true}
	snapLoad       = detector.SystemSnapshot{OnAC: true, GPUDeviceUtilPct: 90}
	snapLowBattery = detector.SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 10}
	snapBatteryOK  = detector.SystemSnapshot{HasBattery: true, OnAC: false, BatteryPct: 80}
)

var (
	sustainTicks  = detector.DefaultConfig().SustainedTicksRequired
	cooldownTicks = detector.DefaultConfig().CooldownTicksRequired
)

// harness wires a Daemon to fakes.
type harness struct {
	d    *Daemon
	gpu  *fakeGPU
	fah  *fakeFAH
	col  *scriptedCollector
	eval *countingEvaluator
	log  *callLog
	logs *syncBuffer
	dir  string
}

func newHarness(t *testing.T, supports bool) *harness {
	t.Helper()
	log := &callLog{}
	h := &harness{
		gpu:  newFakeGPU(log, supports),
		fah:  newFakeFAH(log),
		col:  &scriptedCollector{snap: snapIdle},
		eval: &countingEvaluator{inner: detector.NewEvaluator(detector.DefaultConfig())},
		log:  log,
		logs: &syncBuffer{},
		dir:  t.TempDir(),
	}
	cfg := DefaultConfig()
	cfg.RuntimeDir = h.dir
	cfg.Logger = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := New(cfg, Deps{GPU: h.gpu, FAH: h.fah, Collector: h.col, Evaluator: h.eval})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	return h
}

// start runs the startup sequence and clears the call log so tests only see
// calls made by ticks.
func (h *harness) start(t *testing.T) {
	t.Helper()
	if err := h.d.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	h.log.reset()
}

// ticks runs n ticks with snap as the collected snapshot.
func (h *harness) ticks(n int, snap detector.SystemSnapshot) {
	h.col.set(snap, nil)
	for i := 0; i < n; i++ {
		h.d.tick(context.Background())
	}
}

// failTicks runs n ticks where Collect returns err.
func (h *harness) failTicks(n int, err error) {
	h.col.set(detector.SystemSnapshot{}, err)
	for i := 0; i < n; i++ {
		h.d.tick(context.Background())
	}
}

func (h *harness) markerPresent(t *testing.T) bool {
	t.Helper()
	ok, err := marker{dir: h.dir}.present()
	if err != nil {
		t.Fatalf("marker.present: %v", err)
	}
	return ok
}

func countLines(s, substr string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}
