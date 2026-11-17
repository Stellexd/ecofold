package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellexd/ecofold/internal/daemon"
	"github.com/Stellexd/ecofold/internal/fah"
	"github.com/Stellexd/ecofold/internal/ipc"
)

// fakeDaemon is an ipc.Handler with a scripted status and scripted failures.
type fakeDaemon struct {
	mu     sync.Mutex
	status Status
	fail   map[string]*ipc.Error // by command
	block  chan struct{}         // when non-nil, every request waits for it
	calls  []ipc.Request
}

func newFakeDaemon() *fakeDaemon {
	d := &fakeDaemon{fail: map[string]*ipc.Error{}}
	s := &d.status
	s.Mode, s.ManualOverride = ModeFolding, "none"
	s.PowerCapable, s.PowerEnabled, s.CapApplied = true, true, true
	s.PowerLimitW, s.DefaultPowerLimitW, s.MinPowerLimitW, s.MaxPowerLimitW, s.EcoWatts = 123, 145, 100, 145, 123
	s.GPU.Name = "Fake RTX 5060"
	s.PauseListSupported = true
	s.GameSessions, s.PauseProcesses, s.PauseMatches = []string{}, []string{}, []string{}
	return d
}

func (d *fakeDaemon) Handle(ctx context.Context, req ipc.Request) ipc.Response {
	d.mu.Lock()
	block := d.block
	d.calls = append(d.calls, req)
	d.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.fail[req.Cmd]; e != nil {
		if req.Cmd == ipc.CmdForceResume {
			d.status.ManualOverride = "none" // the real daemon clears it even when it refuses
		}
		return ipc.Fail(e.Code, e.Msg)
	}
	s := &d.status
	switch req.Cmd {
	case ipc.CmdForcePause:
		s.Mode, s.ManualOverride, s.PausedByUs, s.PowerLimitW = ModePausedManual, OverridePause, true, s.DefaultPowerLimitW
		s.FAH.Paused, s.FAH.Reachable, s.FAH.ObservedAt = true, true, time.Now()
	case ipc.CmdForceResume:
		s.Mode, s.ManualOverride, s.PausedByUs, s.PowerLimitW = ModeFolding, "none", false, s.EcoWatts
		s.FAH.Paused, s.FAH.Reachable, s.FAH.ObservedAt = false, true, time.Now()
	case ipc.CmdSetEcoWatts:
		var a struct {
			Watts int `json:"watts"`
		}
		_ = json.Unmarshal(req.Args, &a)
		s.EcoWatts, s.PowerLimitW = a.Watts, a.Watts
	case ipc.CmdSetPauseProcesses:
		var a struct {
			Patterns []string `json:"patterns"`
		}
		_ = json.Unmarshal(req.Args, &a)
		s.PauseProcesses = a.Patterns
	}
	s.UpdatedAt = time.Now()
	return ipc.OK(*s)
}

func (d *fakeDaemon) set(fn func(d *fakeDaemon)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

// commands returns the commands received, without the status polls.
func (d *fakeDaemon) commands() []ipc.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []ipc.Request
	for _, r := range d.calls {
		if r.Cmd != ipc.CmdStatus {
			out = append(out, r)
		}
	}
	return out
}

// socketPath returns a short path: t.TempDir() overflows sun_path on macOS.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "ecofold.sock")
}

// startDaemon serves d on path with a real ipc.Server and returns its stop
// function.
func startDaemon(t *testing.T, path string, d *fakeDaemon) (stop func()) {
	t.Helper()
	srv, err := ipc.Listen(ipc.Config{Path: path, Handler: d, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("ipc.Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}
	t.Cleanup(stop)
	return stop
}

func newController(path string) *Controller {
	return New(Config{SocketPath: path, Poll: 10 * time.Millisecond, Timeout: time.Second})
}

var bg = context.Background()

// TestReconnect: the controller turns Disconnected when the daemon goes
// away and recovers by itself when it comes back.
func TestReconnect(t *testing.T) {
	path := socketPath(t)
	c := newController(path)

	if v := c.View(); v.Connected || v.Tone() != ToneGray || v.Headline() != "Daemon Offline" {
		t.Fatalf("vista inicial = %+v", v)
	}
	c.Refresh(bg)
	if v := c.View(); v.Connected || v.Offline != "Daemon not running" || v.OfflineHint == "" {
		t.Fatalf("sin daemon: %+v", v)
	}

	d := newFakeDaemon()
	stop := startDaemon(t, path, d)
	c.Refresh(bg)
	v := c.View()
	if !v.Connected || v.Offline != "" || v.Status.Mode != ModeFolding || v.Status.GPU.Name != "Fake RTX 5060" {
		t.Fatalf("con daemon: %+v", v)
	}
	if v.Tone() != ToneGreen || v.Headline() != "Folding — 123 W" || v.ToggleLabel() != "Pause Folding" {
		t.Fatalf("vista = %s / %s / %s", v.Tone().Name(), v.Headline(), v.ToggleLabel())
	}

	stop()
	c.Refresh(bg)
	if v := c.View(); v.Connected || v.Offline != "Daemon not running" || v.Tone() != ToneGray {
		t.Fatalf("daemon apagado: %+v", v)
	}
	// An action while it is down fails cleanly and says nothing was sent.
	c.Pause(bg)
	if v := c.View(); v.Connected || !v.MessageIsError || !strings.Contains(v.Message, "Not sent") {
		t.Fatalf("acción sin daemon: %+v", v)
	}

	startDaemon(t, path, d)
	c.Refresh(bg)
	if v := c.View(); !v.Connected || v.Status.Mode != ModeFolding {
		t.Fatalf("daemon de vuelta: %+v", v)
	}
	c.Pause(bg)
	if v := c.View(); v.MessageIsError || v.Status.ManualOverride != OverridePause {
		t.Fatalf("acción tras reconectar: %+v", v)
	}
}

// TestRunFollowsTheDaemon drives the same cycle through the polling loop and
// the OnChange callback, as the UI does.
func TestRunFollowsTheDaemon(t *testing.T) {
	path := socketPath(t)
	changes := make(chan View, 256)
	c := New(Config{SocketPath: path, Poll: 10 * time.Millisecond, Timeout: time.Second,
		OnChange: func(v View) {
			select {
			case changes <- v:
			default:
			}
		}})
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	wait := func(what string, ok func(View) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case v := <-changes:
				if ok(v) {
					return
				}
			case <-deadline:
				t.Fatalf("no se llegó a %q; vista = %+v", what, c.View())
			}
		}
	}

	wait("sin daemon", func(v View) bool { return !v.Connected && v.Offline == "Daemon not running" })
	d := newFakeDaemon()
	stop := startDaemon(t, path, d)
	wait("conectado", func(v View) bool { return v.Connected && v.Status.Mode == ModeFolding })

	d.set(func(d *fakeDaemon) { d.status.Mode, d.status.PowerLimitW = ModePausedLoad, 145 })
	wait("pausa por juego", func(v View) bool {
		return v.Tone() == ToneBlue && v.Headline() == "Paused (Game) — 145 W"
	})

	stop()
	wait("desconectado", func(v View) bool { return !v.Connected })
	startDaemon(t, path, d)
	wait("reconectado", func(v View) bool { return v.Connected })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run no terminó al cancelar")
	}
}

// TestActionsAreDispatched checks each action sends the right command and
// args, and that the response's status reaches the view.
func TestActionsAreDispatched(t *testing.T) {
	path := socketPath(t)
	d := newFakeDaemon()
	startDaemon(t, path, d)
	c := newController(path)
	c.Refresh(bg)

	c.Pause(bg)
	v := c.View()
	if v.Status.ManualOverride != OverridePause || v.Message != "Folding paused" || v.MessageIsError || v.MessageAt.IsZero() {
		t.Fatalf("tras Pause: %+v", v)
	}
	if v.Tone() != ToneGray || v.ToggleLabel() != "Resume Folding" || v.Headline() != "Paused (Manual) — 145 W" {
		t.Fatalf("vista = %s / %s / %s", v.Tone().Name(), v.ToggleLabel(), v.Headline())
	}

	c.Toggle(bg) // resumes: a manual pause is active
	if v := c.View(); v.Status.ManualOverride != "none" || v.Message != "Folding resumed" || v.ToggleLabel() != "Pause Folding" {
		t.Fatalf("tras Toggle: %+v", v)
	}
	c.Toggle(bg) // pauses again
	c.Resume(bg)

	c.SetEcoWatts(bg, 110)
	if v := c.View(); v.Status.EcoWatts != 110 || v.Message != "Eco limit set to 110 W" {
		t.Fatalf("tras SetEcoWatts: %+v", v)
	}

	c.SetPauseProcesses(bg, " blender , obs*\nresolve,, ")
	if v := c.View(); v.PauseListText() != "blender, obs*, resolve" || v.Message != "Pause list saved" {
		t.Fatalf("tras SetPauseProcesses: %+v", v)
	}
	c.SetPauseProcesses(bg, "  ")
	if v := c.View(); v.PauseListText() != "" || v.Message != "Pause list cleared" {
		t.Fatalf("tras borrar la lista: %+v", v)
	}

	var got []string
	for _, r := range d.commands() {
		got = append(got, r.Cmd+string(r.Args))
	}
	want := []string{
		"force_pause", "force_resume", "force_pause", "force_resume",
		`set_eco_watts{"watts":110}`,
		`set_pause_processes{"patterns":["blender","obs*","resolve"]}`,
		`set_pause_processes{"patterns":[]}`,
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("comandos enviados:\n got %v\nwant %v", got, want)
	}
}

// TestRemoteErrorsBecomeMessages: a refusal from the daemon is a message in
// the status bar, never a disconnect.
func TestRemoteErrorsBecomeMessages(t *testing.T) {
	path := socketPath(t)
	d := newFakeDaemon()
	startDaemon(t, path, d)
	c := newController(path)
	c.Refresh(bg)

	cases := []struct {
		cmd, code, msg string
		act            func()
		want           string
	}{
		{ipc.CmdForceResume, ipc.CodeGameActive, "x", func() { c.Resume(bg) }, "a game or GPU load is active"},
		{ipc.CmdForceResume, ipc.CodeOnBattery, "x", func() { c.Resume(bg) }, "on battery"},
		{ipc.CmdForceResume, ipc.CodeCapFailed, "x", func() { c.Resume(bg) }, "rejected the power limit"},
		{ipc.CmdForceResume, ipc.CodeFAHUnavailable, "x", func() { c.Resume(bg) }, "Folding@home did not respond"},
		{ipc.CmdForcePause, ipc.CodeBusy, "x", func() { c.Pause(bg) }, "busy"},
		{ipc.CmdForcePause, ipc.CodeStopping, "x", func() { c.Pause(bg) }, "shutting down"},
		{ipc.CmdSetEcoWatts, ipc.CodeOutOfRange, "x", func() { c.SetEcoWatts(bg, 130) }, "outside what this GPU allows"},
		{ipc.CmdSetEcoWatts, ipc.CodeUnsupported, "x", func() { c.SetEcoWatts(bg, 130) }, "Not supported"},
		{ipc.CmdSetPauseProcesses, ipc.CodeUnsupported, "x", func() { c.SetPauseProcesses(bg, "blender") }, "Not supported"},
		{ipc.CmdSetPauseProcesses, ipc.CodeBadRequest, `pause pattern "fah*" matches Folding@home`,
			func() { c.SetPauseProcesses(bg, "fah*") }, `Rejected: pause pattern "fah*" matches Folding@home`},
		{ipc.CmdForcePause, "brand_new_code", "details", func() { c.Pause(bg) }, "Daemon error (brand_new_code): details"},
	}
	for _, tc := range cases {
		d.set(func(d *fakeDaemon) {
			d.fail = map[string]*ipc.Error{tc.cmd: {Code: tc.code, Msg: tc.msg}}
		})
		tc.act()
		v := c.View()
		if !v.MessageIsError || !strings.Contains(v.Message, tc.want) {
			t.Errorf("%s/%s: mensaje = %q, se esperaba %q", tc.cmd, tc.code, v.Message, tc.want)
		}
		if !v.Connected {
			t.Errorf("%s/%s: la vista pasó a desconectada por un error remoto", tc.cmd, tc.code)
		}
	}

	// A refused force_resume still cleared the manual pause in the daemon:
	// the view must show the refreshed state, not the one before the action.
	d.set(func(d *fakeDaemon) { d.fail = map[string]*ipc.Error{} })
	c.Pause(bg)
	d.set(func(d *fakeDaemon) {
		d.status.Mode = ModePausedLoad
		d.fail[ipc.CmdForceResume] = &ipc.Error{Code: ipc.CodeGameActive, Msg: "x"}
	})
	c.Resume(bg)
	if v := c.View(); v.Status.ManualOverride != "none" || v.Status.Mode != ModePausedLoad || v.Tone() != ToneBlue {
		t.Fatalf("estado tras un force_resume rechazado: %+v", v.Status)
	}
}

// TestEcoWattsRangeIsCheckedLocally: an out-of-range value never leaves the
// client.
func TestEcoWattsRangeIsCheckedLocally(t *testing.T) {
	path := socketPath(t)
	d := newFakeDaemon()
	startDaemon(t, path, d)
	c := newController(path)
	c.Refresh(bg)

	for _, w := range []int{99, 146, 0, -1} {
		c.SetEcoWatts(bg, w)
		if v := c.View(); !v.MessageIsError || v.Message != "Eco limit must be between 100 and 145 W" {
			t.Fatalf("%d W: mensaje = %q", w, v.Message)
		}
	}
	d.set(func(d *fakeDaemon) { d.status.PowerCapable = false })
	c.Refresh(bg)
	c.SetEcoWatts(bg, 120)
	if v := c.View(); !v.MessageIsError || !strings.Contains(v.Message, "no power limit control") {
		t.Fatalf("sin control de potencia: %q", v.Message)
	}
	if got := d.commands(); len(got) != 0 {
		t.Fatalf("se enviaron %d comandos fuera de rango", len(got))
	}
}

// TestAccessDenied covers both ways the daemon keeps a user out: the socket
// file's mode, and hanging up on a peer it does not authorize.
func TestAccessDenied(t *testing.T) {
	t.Run("permisos del socket", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignora los permisos del archivo")
		}
		path := socketPath(t)
		startDaemon(t, path, newFakeDaemon())
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		c := newController(path)
		c.Refresh(bg)
		if v := c.View(); v.Connected || v.Offline != "Access denied" || !strings.Contains(v.OfflineHint, "ecofold") {
			t.Fatalf("vista = %+v", v)
		}
	})

	t.Run("conexión cerrada sin respuesta", func(t *testing.T) {
		path := socketPath(t)
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn.Close() // what the daemon does to an unauthorized peer
			}
		}()
		c := newController(path)
		for i := 0; i < 3; i++ {
			c.Refresh(bg)
			if v := c.View(); v.Connected || v.Offline != "Access denied" {
				t.Fatalf("intento %d: vista = %+v", i, v)
			}
		}
	})
}

// TestUnresponsiveDaemon: a daemon that accepts and never answers costs one
// timeout per request, and the client recovers when it answers again.
func TestUnresponsiveDaemon(t *testing.T) {
	path := socketPath(t)
	d := newFakeDaemon()
	block := make(chan struct{})
	d.block = block
	startDaemon(t, path, d)
	c := New(Config{SocketPath: path, Timeout: 150 * time.Millisecond})

	start := time.Now()
	c.Refresh(bg)
	if v := c.View(); v.Connected || v.Offline != "Daemon not responding" {
		t.Fatalf("vista = %+v", v)
	}
	c.Pause(bg)
	if v := c.View(); !v.MessageIsError || !strings.Contains(v.Message, "No answer from the daemon") {
		t.Fatalf("mensaje = %q", v.Message)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("dos requests con timeout de 150 ms tardaron %v", el)
	}

	close(block)
	d.set(func(d *fakeDaemon) { d.block = nil })
	c.Refresh(bg)
	if v := c.View(); !v.Connected {
		t.Fatalf("no se recuperó: %+v", v)
	}
}

// TestGarbageFromTheSocket: whatever answers on the socket, the client does
// not panic and reports it as unreachable.
func TestGarbageFromTheSocket(t *testing.T) {
	for name, reply := range map[string]string{
		"no es json":          "hello\n",
		"json de otra cosa":   `{"jsonrpc":"2.0"}` + "\n",
		"status ilegible":     `{"v":1,"id":"1","ok":true,"data":"not an object"}` + "\n",
		"binario":             "\x00\xff\xfe\n",
		"respuesta sin fin":   strings.Repeat("a", 1<<20),
		"versión desconocida": `{"v":9,"id":"1","ok":true,"data":{}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := socketPath(t)
			ln, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer conn.Close()
						buf := make([]byte, 4096)
						_, _ = conn.Read(buf)
						_, _ = conn.Write([]byte(reply))
					}()
				}
			}()
			c := New(Config{SocketPath: path, Timeout: 500 * time.Millisecond})
			c.Refresh(bg)
			c.Pause(bg)
			if v := c.View(); v.Connected || v.Offline == "" {
				t.Fatalf("vista = %+v", v)
			}
		})
	}
}

// TestConcurrentUse hammers the controller from several goroutines while the
// polling loop runs. Run it with -race.
func TestConcurrentUse(t *testing.T) {
	path := socketPath(t)
	d := newFakeDaemon()
	startDaemon(t, path, d)
	var seen sync.Map
	c := New(Config{SocketPath: path, Poll: time.Millisecond, Timeout: time.Second,
		OnChange: func(v View) { seen.Store(v.Status.Mode, true) }})
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch (i + j) % 5 {
				case 0:
					c.Pause(ctx)
				case 1:
					c.Resume(ctx)
				case 2:
					c.SetEcoWatts(ctx, 100+j)
				case 3:
					c.SetPauseProcesses(ctx, "blender")
				case 4:
					_ = c.View().Headline()
				}
			}
		}(i)
	}
	wg.Wait()
	cancel()
	<-done
	if v := c.View(); !v.Connected {
		t.Fatalf("vista final desconectada: %+v", v)
	}
}

// TestStatusMatchesTheDaemonWireFormat guards against drift: every field the
// daemon publishes must decode into the client's Status.
func TestStatusMatchesTheDaemonWireFormat(t *testing.T) {
	src := daemon.Status{
		Mode: daemon.ModePausedManual, ManualOverride: daemon.OverridePause, PausedByUs: true, Degraded: true,
		PowerCapable: true, PowerEnabled: true, CapApplied: true,
		PowerLimitW: 123, DefaultPowerLimitW: 145, MinPowerLimitW: 100, MaxPowerLimitW: 145, EcoWatts: 123,
		GPU: daemon.GPUStatus{Name: "RTX", TempC: 60, PowerUsageW: 110, UsedVRAMMiB: 800, TotalVRAMMiB: 8151,
			UtilPct: 24, InUseMemMiB: 1234},
		Battery: daemon.BatteryStatus{Present: true, OnAC: true, Charging: true, Pct: 77},
		FAH: daemon.FAHStatus{Paused: true, State: fah.StatePaused, ActiveProjectID: 18264, Reachable: true,
			ObservedAt: time.Unix(1700000000, 0)},
		GameSessions:       []string{"steam:1"},
		PauseListSupported: true,
		PauseProcesses:     []string{"blender"},
		PauseMatches:       []string{"blender"},
		UpdatedAt:          time.Unix(1700000001, 0),
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var got Status
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("el cliente no conoce un campo del daemon: %v\n%s", err, raw)
	}
	back, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(raw) {
		t.Fatalf("el status no sobrevive el viaje de ida y vuelta:\ndaemon  %s\ncliente %s", raw, back)
	}
	if got.Mode != ModePausedManual || got.ManualOverride != OverridePause {
		t.Fatalf("constantes desalineadas: %q / %q", got.Mode, got.ManualOverride)
	}
	if FAHStateIdle != fah.StateIdle {
		t.Errorf("estado idle del cliente %q, de fah %q", FAHStateIdle, fah.StateIdle)
	}
	for client, server := range map[string]daemon.Mode{
		ModeStarting: daemon.ModeStarting, ModeFolding: daemon.ModeFolding, ModeFoldingEco: daemon.ModeFoldingEco,
		ModePausedLoad: daemon.ModePausedLoad, ModePausedBattery: daemon.ModePausedBattery,
		ModePausedManual: daemon.ModePausedManual, ModeDegraded: daemon.ModeDegraded,
	} {
		if client != string(server) {
			t.Errorf("modo del cliente %q, del daemon %q", client, server)
		}
	}
}

func TestViewLabels(t *testing.T) {
	mk := func(mode string, edit func(*Status)) View {
		v := View{Connected: true}
		v.Status.Mode, v.Status.ManualOverride = mode, "none"
		v.Status.PowerCapable, v.Status.PowerLimitW, v.Status.DefaultPowerLimitW = true, 123, 145
		if edit != nil {
			edit(&v.Status)
		}
		return v
	}
	cases := []struct {
		name     string
		v        View
		tone     Tone
		headline string
		toggle   string
	}{
		{"folding", mk(ModeFolding, nil), ToneGreen, "Folding — 123 W", "Pause Folding"},
		{"folding_eco sin control de potencia", mk(ModeFoldingEco, func(s *Status) { s.PowerCapable = false }),
			ToneGreen, "Folding (on battery)", "Pause Folding"},
		{"juego", mk(ModePausedLoad, func(s *Status) { s.PowerLimitW = 145 }), ToneBlue, "Paused (Game) — 145 W", "Pause Folding"},
		{"batería", mk(ModePausedBattery, func(s *Status) { s.PowerCapable = false }), ToneAmber, "Paused (Battery)", "Pause Folding"},
		{"manual", mk(ModePausedManual, func(s *Status) { s.ManualOverride, s.PowerLimitW = OverridePause, 145 }),
			ToneGray, "Paused (Manual) — 145 W", "Resume Folding"},
		{"manual durante un juego", mk(ModePausedLoad, func(s *Status) { s.ManualOverride, s.PowerLimitW = OverridePause, 145 }),
			ToneBlue, "Paused (Game) — 145 W", "Resume Folding"},
		{"pausa del usuario en FAH", mk(ModeFolding, func(s *Status) { s.FAH.Paused, s.FAH.Reachable = true, true }),
			ToneGreen, "Folding — 123 W", "Resume Folding"},
		{"FAH caído", mk(ModeFolding, func(s *Status) { s.FAH.ObservedAt = time.Now() }),
			ToneGray, "Folding@home Not Reachable — 123 W", "Pause Folding"},
		{"FAH caído en batería", mk(ModeFoldingEco, func(s *Status) { s.PowerCapable, s.FAH.ObservedAt = false, time.Now() }),
			ToneGray, "Folding@home Not Reachable", "Pause Folding"},
		{"FAH caído durante un juego", mk(ModePausedLoad, func(s *Status) { s.PowerLimitW, s.FAH.ObservedAt = 145, time.Now() }),
			ToneBlue, "Paused (Game) — 145 W", "Pause Folding"},
		{"degraded", mk(ModeDegraded, func(s *Status) { s.PowerLimitW = 145 }), ToneGray, "Degraded — 145 W", "Pause Folding"},
		{"starting", mk(ModeStarting, func(s *Status) { s.PowerLimitW = 0 }), ToneGray, "Starting", "Pause Folding"},
		{"modo futuro", mk("hibernating", nil), ToneGray, "hibernating — 123 W", "Pause Folding"},
		{"offline", View{Offline: "Daemon not running"}, ToneGray, "Daemon Offline", "Pause Folding"},
	}
	for _, tc := range cases {
		if tc.v.Tone() != tc.tone || tc.v.Headline() != tc.headline || tc.v.ToggleLabel() != tc.toggle {
			t.Errorf("%s: %s / %q / %q; se esperaba %s / %q / %q", tc.name,
				tc.v.Tone().Name(), tc.v.Headline(), tc.v.ToggleLabel(), tc.tone.Name(), tc.headline, tc.toggle)
		}
	}

	v := mk(ModeFolding, func(s *Status) { s.Battery.Present, s.Battery.Pct = true, 53 })
	if got := v.BatteryLabel(); got != "53 %, on battery" {
		t.Errorf("batería = %q", got)
	}
	v.Status.Battery.OnAC = true
	if got := v.BatteryLabel(); got != "53 %, on AC" {
		t.Errorf("batería = %q", got)
	}
	v.Status.Battery.Charging = true
	if got := v.BatteryLabel(); got != "53 %, charging" {
		t.Errorf("batería = %q", got)
	}
	if got := mk(ModeFolding, nil).BatteryLabel(); got != "" {
		t.Errorf("batería sin batería = %q", got)
	}
	seen := func(edit func(*Status)) View {
		return mk(ModeFolding, func(s *Status) {
			s.FAH.Reachable, s.FAH.ObservedAt, s.FAH.State = true, time.Now(), "folding"
			edit(s)
		})
	}
	for _, tc := range []struct {
		name string
		v    View
		want string
	}{
		{"sin consultar", mk(ModeFolding, nil), "Folding@home: not checked yet"},
		{"caído", seen(func(s *Status) { s.FAH.Reachable = false }), "Folding@home: not reachable (is the client running?)"},
		{"con proyecto", seen(func(s *Status) { s.FAH.ActiveProjectID = 18264 }), "Folding@home: running (project #18264)"},
		{"sin proyecto", seen(func(*Status) {}), "Folding@home: running"},
		{"idle", seen(func(s *Status) { s.FAH.State = FAHStateIdle }), "Folding@home: idle (waiting for work unit)"},
		{"pausa del usuario", seen(func(s *Status) { s.FAH.Paused, s.FAH.State = true, "paused" }), "Folding@home: paused"},
		{"pausa de EcoFold", seen(func(s *Status) { s.FAH.Paused, s.PausedByUs = true, true }), "Folding@home: paused by EcoFold"},
	} {
		if got := tc.v.FAHLabel(); got != tc.want {
			t.Errorf("FAH %s = %q, se esperaba %q", tc.name, got, tc.want)
		}
	}

	nvidia := mk(ModeFolding, func(s *Status) {
		s.GPU.PowerUsageW, s.GPU.TempC, s.GPU.UsedVRAMMiB, s.GPU.TotalVRAMMiB = 118, 61, 812, 8151
	})
	if got := nvidia.UsageLabel(); got != "Drawing 118 W · 61 °C · VRAM 812 / 8151 MiB" {
		t.Errorf("uso = %q", got)
	}
	// No permission: power control is off, the telemetry is still the card's.
	nvidia.Status.PowerCapable = false
	if got := nvidia.UsageLabel(); got != "Drawing 118 W · 61 °C · VRAM 812 / 8151 MiB" {
		t.Errorf("uso sin permiso = %q", got)
	}
	mac := mk(ModeFoldingEco, func(s *Status) { s.PowerCapable, s.GPU.UtilPct, s.GPU.InUseMemMiB = false, 24, 1234 })
	if got := mac.UsageLabel(); got != "GPU utilization: 24 % · In-use memory: 1234 MiB" {
		t.Errorf("uso en macOS = %q", got)
	}
}

// TestIcons: four distinct PNGs, drawn in memory, with a transparent corner,
// the tone's color on the disc and the white bolt in the middle.
func TestIcons(t *testing.T) {
	seen := map[string]Tone{}
	for _, tone := range []Tone{ToneGreen, ToneBlue, ToneAmber, ToneGray} {
		raw := IconPNG(tone)
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", tone.Name(), err)
		}
		if b := img.Bounds(); b.Dx() != iconSize || b.Dy() != iconSize {
			t.Fatalf("%s: tamaño %v", tone.Name(), b)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%s: la esquina no es transparente", tone.Name())
		}
		want := tone.Color()
		r, g, b, a := img.At(8, 32).RGBA()
		if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(b>>8) != want.B || a != 0xffff {
			t.Errorf("%s: el disco es %02x%02x%02x, se esperaba %02x%02x%02x", tone.Name(), r>>8, g>>8, b>>8, want.R, want.G, want.B)
		}
		if r, g, b, _ := img.At(32, 32).RGBA(); r != 0xffff || g != 0xffff || b != 0xffff {
			t.Errorf("%s: el centro no es el rayo blanco", tone.Name())
		}
		if prev, dup := seen[string(raw)]; dup {
			t.Errorf("%s y %s tienen el mismo ícono", tone.Name(), prev.Name())
		}
		seen[string(raw)] = tone
		if &IconPNG(tone)[0] != &raw[0] {
			t.Errorf("%s: el ícono no está en caché", tone.Name())
		}
	}
}
