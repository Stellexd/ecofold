//go:build darwin

package detector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

// synthClient is one user client of a synthetic IOAccelerator reading.
// usage is the raw AppUsage XML; empty means one entry with gpuTime.
type synthClient struct {
	id      uint64
	creator string
	gpuTime uint64
	usage   string
	noUsage bool
}

// clientsPlist builds a minimal `ioreg -a -r -d 2 -l -c IOAccelerator`
// reading: one accelerator at 40 % with the given user clients.
func clientsPlist(clients ...synthClient) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array><dict>` +
		`<key>IOClass</key><string>AGXAcceleratorG16G</string><key>model</key><string>Apple M4</string>` +
		`<key>PerformanceStatistics</key><dict><key>Device Utilization %</key><integer>40</integer>` +
		`<key>In use system memory</key><integer>734003200</integer></dict>` +
		`<key>IORegistryEntryChildren</key><array>`)
	for _, c := range clients {
		fmt.Fprintf(&b, `<dict><key>IORegistryEntryID</key><integer>%d</integer>`, c.id)
		fmt.Fprintf(&b, `<key>IOUserClientCreator</key><string>%s</string>`, c.creator)
		switch {
		case c.noUsage:
		case c.usage != "":
			fmt.Fprintf(&b, `<key>AppUsage</key>%s`, c.usage)
		default:
			fmt.Fprintf(&b, `<key>AppUsage</key><array><dict><key>API</key><string>Metal</string>`+
				`<key>accumulatedGPUTime</key><integer>%d</integer></dict></array>`, c.gpuTime)
		}
		b.WriteString(`</dict>`)
	}
	b.WriteString(`</array></dict></array></plist>`)
	return []byte(b.String())
}

// clientsScript is a fetchClients seam that serves readings in order and a
// clock that advances 2 s per reading.
type clientsScript struct {
	readings [][]byte
	errs     []error
	calls    int
	at       time.Time
}

func (s *clientsScript) fetch(context.Context) ([]byte, error) {
	i := min(s.calls, len(s.readings)-1)
	s.calls++
	s.at = s.at.Add(2 * time.Second)
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return s.readings[i], nil
}

func (s *clientsScript) now() time.Time { return s.at }

func gpuAppsCollector(t *testing.T, s *clientsScript) (*DarwinCollector, *scriptedIOReg) {
	t.Helper()
	io := &scriptedIOReg{tb: t, battery: "m4_air_battery_charging.plist", gpu: "m4_air_gpu_idle.plist"}
	c := darwinCollectorWith(io)
	c.fetchClients, c.now = s.fetch, s.now
	c.execName = func(int32) (string, error) { return "", errors.New("no procargs in tests") }
	return c, io
}

func collectApps(t *testing.T, c *DarwinCollector) []GPUAppUsage {
	t.Helper()
	snap, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return snap.GPUApps
}

// TestGPUClientsRealFixture: the sanitized M4 Air readings parse, and the
// global metrics read from them equal those of the same node without its
// clients, so the detailed reading does not change the global path.
func TestGPUClientsRealFixture(t *testing.T) {
	raw := readFixture(t, "m4_air_gpu_clients_tick1.plist")
	clients, ok, err := parseGPUClients(raw)
	if err != nil || !ok {
		t.Fatalf("parseGPUClients: ok=%v, err=%v", ok, err)
	}
	if len(clients) != 14 {
		t.Fatalf("%d clientes, se esperaban 14", len(clients))
	}
	ws := 0
	for _, cl := range clients {
		if cl.Name == "WindowServer" {
			ws++
		}
		if cl.PID < 1000 || cl.ID == 0 {
			t.Fatalf("cliente = %+v", cl)
		}
	}
	if ws != 2 {
		t.Fatalf("WindowServer tiene %d clientes, se esperaban 2", ws)
	}

	var nodes []map[string]any
	if _, err := plist.Unmarshal(raw, &nodes); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		delete(n, "IORegistryEntryChildren")
	}
	stripped, err := plist.MarshalIndent(nodes, plist.XMLFormat, "\t")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stripped, []byte("AppUsage")) {
		t.Fatal("no se quitaron los clientes")
	}
	withClients, err1 := ParseIOAcceleratorPlist(raw)
	plain, err2 := ParseIOAcceleratorPlist(stripped)
	if err1 != nil || err2 != nil || withClients != plain || plain.GPUModel != "Apple M4" {
		t.Fatalf("métricas globales distintas: %+v (%v) vs %+v (%v)", withClients, err1, plain, err2)
	}
}

// TestDarwinCollectorGPUAppsFromFixtures: two real readings 2 s apart give
// per-app GPU time; system processes are left out. Between them only
// WindowServer (ignored) and VSCodium Helper drew on the GPU.
func TestDarwinCollectorGPUAppsFromFixtures(t *testing.T) {
	t1, t2 := readFixture(t, "m4_air_gpu_clients_tick1.plist"), readFixture(t, "m4_air_gpu_clients_tick2.plist")
	s := &clientsScript{readings: [][]byte{t1, t2}}
	c, io := gpuAppsCollector(t, s)

	snap, err := c.Collect(t.Context())
	if err != nil || snap.GPUApps != nil {
		t.Fatalf("primera lectura: apps=%v, err=%v", snap.GPUApps, err)
	}
	plain, _ := ParseIOAcceleratorPlist(t1)
	if snap.GPUDeviceUtilPct != plain.GPUDeviceUtilPct || snap.GPUModel != "Apple M4" {
		t.Fatalf("métricas globales = %d %q", snap.GPUDeviceUtilPct, snap.GPUModel)
	}
	apps := collectApps(t, c)

	// Expected from the fixtures themselves.
	c1, _, _ := parseGPUClients(t1)
	c2, _, _ := parseGPUClients(t2)
	before := map[uint64]uint64{}
	for _, cl := range c1 {
		before[cl.ID] = cl.GPUTime
	}
	want := map[string]uint64{}
	for _, cl := range c2 {
		if d := cl.GPUTime - before[cl.ID]; d > 0 && !matchPattern(darwinIgnoredGPUApps, cl.Name) {
			want[cl.Name] += d
		}
	}
	if len(apps) == 0 || len(apps) != len(want) {
		t.Fatalf("apps = %+v, se esperaban %v", apps, want)
	}
	for i, a := range apps {
		ns, ok := want[a.Name]
		if !ok {
			t.Fatalf("app inesperada %+v", a)
		}
		if pct := int(min(float64(ns)/2e9*100, 100) + 0.5); a.GPUTimePct != pct {
			t.Fatalf("%s: %d %%, se esperaba %d %%", a.Name, a.GPUTimePct, pct)
		}
		if i > 0 && apps[i-1].GPUTimePct < a.GPUTimePct {
			t.Fatalf("no ordenado: %+v", apps)
		}
	}
	if io.calls["IOAccelerator"] != 0 {
		t.Fatalf("ioreg simple llamado %d veces con la lectura detallada sana", io.calls["IOAccelerator"])
	}
	t.Logf("apps = %+v", apps)
}

func TestGPUAppsDeltas(t *testing.T) {
	const s = uint64(time.Second)
	readings := [][]byte{
		clientsPlist(
			synthClient{id: 1, creator: "pid 100, Game", gpuTime: 10 * s},
			synthClient{id: 2, creator: "pid 100, Game", gpuTime: 0},
			synthClient{id: 3, creator: "pid 200, WindowServer", gpuTime: 5 * s},
			synthClient{id: 4, creator: "pid 300, Reused", gpuTime: 7 * s},
			synthClient{id: 5, creator: "pid 400, Backwards", gpuTime: 9 * s},
			synthClient{id: 6, creator: "pid 500, Gone", gpuTime: 1 * s},
			synthClient{id: 7, creator: fmt.Sprintf("pid %d, ecofold-daemon", os.Getpid()), gpuTime: 1 * s},
			synthClient{id: 8, creator: "pid 600, Idle", gpuTime: 3 * s},
		),
		clientsPlist(
			synthClient{id: 1, creator: "pid 100, Game", gpuTime: 11 * s},                      // +1 s
			synthClient{id: 2, creator: "pid 100, Game", gpuTime: s / 2},                       // +0.5 s, same app
			synthClient{id: 3, creator: "pid 200, WindowServer", gpuTime: 7 * s},               // ignored
			synthClient{id: 4, creator: "pid 301, Reused", gpuTime: 8 * s},                     // PID changed: new
			synthClient{id: 9, creator: "pid 300, Reused", gpuTime: 1 * s},                     // reused PID, new client
			synthClient{id: 5, creator: "pid 400, Backwards", gpuTime: 1 * s},                  // went back: new
			synthClient{id: 7, creator: fmt.Sprintf("pid %d, x", os.Getpid()), gpuTime: 2 * s}, // self
			synthClient{id: 8, creator: "pid 600, Idle", gpuTime: 3 * s},                       // no work
			synthClient{id: 10, creator: "pid 700, Busy", gpuTime: 900 * s},                    // first seen
		),
		clientsPlist(
			synthClient{id: 10, creator: "pid 700, Busy", gpuTime: 910 * s}, // 10 s in 2 s: clamped
		),
	}
	sc := &clientsScript{readings: readings}
	c, _ := gpuAppsCollector(t, sc)
	if apps := collectApps(t, c); apps != nil {
		t.Fatalf("primera lectura: %+v", apps)
	}
	apps := collectApps(t, c)
	if len(apps) != 1 || apps[0] != (GPUAppUsage{PID: 100, Name: "Game", GPUTimePct: 75}) {
		t.Fatalf("apps = %+v", apps)
	}
	apps = collectApps(t, c)
	if len(apps) != 1 || apps[0] != (GPUAppUsage{PID: 700, Name: "Busy", GPUTimePct: 100}) {
		t.Fatalf("apps = %+v", apps)
	}
}

// TestGPUAppsResolvesTruncatedNames: a 16-byte creator name is replaced by
// the executable name when the process is still there.
func TestGPUAppsResolvesTruncatedNames(t *testing.T) {
	r1 := clientsPlist(synthClient{id: 1, creator: "pid 100, Final Cut Camera", gpuTime: 0},
		synthClient{id: 2, creator: "pid 101, NotificationCent", gpuTime: 0})
	r2 := clientsPlist(synthClient{id: 1, creator: "pid 100, Final Cut Camera", gpuTime: 2e8},
		synthClient{id: 2, creator: "pid 101, NotificationCent", gpuTime: 2e8})
	c, _ := gpuAppsCollector(t, &clientsScript{readings: [][]byte{r1, r2}})
	c.execName = func(pid int32) (string, error) {
		if pid == 100 {
			return "Final Cut Camera Pro", nil
		}
		return "", errors.New("gone")
	}
	collectApps(t, c)
	apps := collectApps(t, c)
	if len(apps) != 1 || apps[0].Name != "Final Cut Camera Pro" || apps[0].GPUTimePct != 10 {
		t.Fatalf("apps = %+v", apps)
	}
}

// TestGPUAppsSchemaMissing: without AppUsage (an older or newer macOS) there
// are no per-app figures, the global metrics still arrive and the tick does
// not fail.
func TestGPUAppsSchemaMissing(t *testing.T) {
	noUsage := clientsPlist(synthClient{id: 1, creator: "pid 100, Game", noUsage: true})
	c, _ := gpuAppsCollector(t, &clientsScript{readings: [][]byte{noUsage}})
	for range 3 {
		snap, err := c.Collect(t.Context())
		if err != nil || snap.GPUApps != nil || snap.GPUDeviceUtilPct != 40 {
			t.Fatalf("snap = %+v, err = %v", snap, err)
		}
	}
	if c.clientsOff {
		t.Fatal("una lectura sana sin AppUsage no debe desactivar la lectura detallada")
	}
}

// TestGPUAppsMalformedClients: a malformed client is skipped, the others
// still count.
func TestGPUAppsMalformedClients(t *testing.T) {
	bad := []synthClient{
		{id: 2, creator: "WindowServer", gpuTime: 1},
		{id: 3, creator: "pid x, Game", gpuTime: 1},
		{id: 4, creator: "pid 0, Game", gpuTime: 1},
		{id: 5, creator: "pid 99999999999, Game", gpuTime: 1},
		{id: 6, creator: "pid 7, bad\x7fname", gpuTime: 1},
		{id: 7, creator: "pid 7, Game", usage: `<string>Metal</string>`},
		{id: 8, creator: "pid 8, Game", usage: `<array><string>x</string></array>`},
		{id: 9, creator: "pid 9, Game", usage: `<array><dict><key>accumulatedGPUTime</key><string>1</string></dict></array>`},
		{id: 10, creator: "pid 10, Game", usage: `<array><dict><key>accumulatedGPUTime</key><integer>-5</integer></dict></array>`},
		{id: 11, creator: "pid 11, Game", usage: `<array><dict><key>accumulatedGPUTime</key><integer>18446744073709551615</integer></dict>` +
			`<dict><key>accumulatedGPUTime</key><integer>2</integer></dict></array>`},
	}
	r1 := clientsPlist(append([]synthClient{{id: 1, creator: "pid 1, Good", gpuTime: 0}}, bad...)...)
	r2 := clientsPlist(append([]synthClient{{id: 1, creator: "pid 1, Good", gpuTime: 1e9}}, bad...)...)
	clients, ok, err := parseGPUClients(r2)
	if err != nil || !ok || len(clients) != 1 || clients[0].Name != "Good" {
		t.Fatalf("clientes = %+v, ok=%v, err=%v", clients, ok, err)
	}
	c, _ := gpuAppsCollector(t, &clientsScript{readings: [][]byte{r1, r2}})
	collectApps(t, c)
	if apps := collectApps(t, c); len(apps) != 1 || apps[0].GPUTimePct != 50 {
		t.Fatalf("apps = %+v", apps)
	}
}

// TestGPUAppsHugeListing: thousands of clients are bounded and the report
// keeps the busiest maxGPUApps apps.
func TestGPUAppsHugeListing(t *testing.T) {
	n := maxGPUClients + 500
	mk := func(scale uint64) []byte {
		cs := make([]synthClient, n)
		for i := range cs {
			cs[i] = synthClient{id: uint64(i + 1), creator: fmt.Sprintf("pid %d, app%d", i+1, i), gpuTime: uint64(i) * scale}
		}
		return clientsPlist(cs...)
	}
	r1, r2 := mk(0), mk(100)
	if len(r2) > maxPlistBytes {
		t.Skipf("la lectura sintética (%d bytes) supera el tope", len(r2))
	}
	c, _ := gpuAppsCollector(t, &clientsScript{readings: [][]byte{r1, r2}})
	collectApps(t, c)
	apps := collectApps(t, c)
	if len(apps) != maxGPUApps {
		t.Fatalf("%d apps, se esperaba el tope %d", len(apps), maxGPUApps)
	}
	if len(c.gpuPrev) > maxGPUClients {
		t.Fatalf("%d clientes guardados", len(c.gpuPrev))
	}
}

// TestGPUAppsFallbackToPlainReading: a failing or unparseable detailed
// reading falls back to the plain one in the same tick, never failing it;
// after clientAttempts failures in a row only the plain reading runs.
func TestGPUAppsFallbackToPlainReading(t *testing.T) {
	good := clientsPlist(synthClient{id: 1, creator: "pid 1, Game", gpuTime: 0})
	s := &clientsScript{
		readings: [][]byte{good, []byte("<plist><array><dict>"), []byte("bplist00garbage"), good},
		errs:     []error{nil, nil, nil, errors.New("ioreg: signal: killed")},
	}
	c, io := gpuAppsCollector(t, s)
	for i := range 6 {
		snap, err := c.Collect(t.Context())
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if snap.GPUApps != nil {
			t.Fatalf("tick %d: apps = %+v", i, snap.GPUApps)
		}
	}
	if s.calls != 1+clientAttempts {
		t.Fatalf("lectura detallada intentada %d veces, se esperaban %d", s.calls, 1+clientAttempts)
	}
	if io.calls["IOAccelerator"] != 5 {
		t.Fatalf("lectura simple %d veces, se esperaban 5", io.calls["IOAccelerator"])
	}

	// A plain reading that also fails does fail the tick, as before.
	c, io = gpuAppsCollector(t, &clientsScript{readings: [][]byte{nil}, errs: []error{errors.New("x")}})
	io.gpuErr = errors.New("ioreg: exit status 1")
	if _, err := c.Collect(t.Context()); err == nil {
		t.Fatal("se esperaba error con ambas lecturas fallidas")
	}
}

func FuzzParseGPUClients(f *testing.F) {
	f.Add(clientsPlist(synthClient{id: 1, creator: "pid 1, Game", gpuTime: 5}))
	f.Add([]byte(`<plist><array><dict><key>IORegistryEntryChildren</key><string/></dict></array></plist>`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		clients, _, err := parseGPUClients(raw)
		if err != nil {
			return
		}
		for _, cl := range clients {
			if cl.PID == 0 || cl.Name == "" || cleanProcName(cl.Name) != cl.Name {
				t.Fatalf("cliente inválido %+v", cl)
			}
		}
	})
}

// TestFetchIOAcceleratorClientsLive reads this Mac's clients and reports
// whether AppUsage is published.
func TestFetchIOAcceleratorClientsLive(t *testing.T) {
	raw, err := FetchIOAcceleratorClientsContext(t.Context())
	if err != nil {
		t.Fatalf("FetchIOAcceleratorClientsContext: %v", err)
	}
	gpu, err := ParseIOAcceleratorPlist(raw)
	if err != nil || gpu.GPUClass == "" {
		t.Fatalf("métricas globales: %+v, %v", gpu, err)
	}
	clients, ok, err := parseGPUClients(raw)
	if err != nil {
		t.Fatalf("parseGPUClients: %v", err)
	}
	t.Logf("%d bytes, AppUsage=%v, %d clientes", len(raw), ok, len(clients))
}
